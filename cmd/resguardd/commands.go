package main

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/daemon"
	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
	"github.com/LucasCis92/resguard/internal/version"
)

var errNoSample = errors.New("todavía no hay muestras; espera un par de segundos")

func registerCommands(srv *daemon.Server, col *collector.Collector, store *events.Store, cur *current) {
	srv.Register(daemon.Command{Name: "ping", Description: "comprueba que el demonio responde",
		Text: func([]string) (string, error) { return "pong", nil }})
	srv.Register(daemon.Command{Name: "version", Description: "versión del demonio",
		Text: func([]string) (string, error) { return version.Version, nil }})
	srv.Register(daemon.Command{Name: "status", Description: "resumen del estado del sistema",
		Text: func([]string) (string, error) { return status(col, store) }})
	srv.Register(daemon.Command{Name: "history", Usage: "history [n]", Description: "últimas n muestras (20 por defecto)",
		Text: func(args []string) (string, error) { return historyCmd(col, args) }})
	srv.Register(daemon.Command{Name: "services", Usage: "services [cpu|mem] [apps|servicios|contenedores] [n]",
		Description: "servicios, aplicaciones y contenedores que más consumen",
		Text:        func(args []string) (string, error) { return servicesCmd(col, args) }})
	srv.Register(daemon.Command{Name: "events", Usage: "events [n]", Description: "últimas n incidencias (20 por defecto)",
		Text: func(args []string) (string, error) { return eventsCmd(store, args) }})
	srv.Register(daemon.Command{Name: "event", Usage: "event <id>", Description: "detalle de una incidencia",
		Text: func(args []string) (string, error) { return eventCmd(store, args) }})

	srv.Register(daemon.Command{Name: "config", Description: "configuración en uso y de dónde se cargó",
		Text: func([]string) (string, error) { return cur.get().describe(), nil }})

	// Procesos de unas unidades, para la vista de procesos de "watch". Solo
	// se aceptan nombres de unidades que el recolector conoce: la ruta del
	// cgroup nunca la decide el cliente.
	procReader := metrics.NewReader() // propio: el del recolector no es concurrente
	var procMu sync.Mutex
	srv.Register(daemon.Command{Name: "processes", Hidden: true,
		Data: func(args []string) (any, error) {
			if len(args) == 0 || len(args) > 20 {
				return nil, errors.New("uso: processes <unidad>...")
			}
			want := map[string]bool{}
			for _, a := range args {
				want[a] = true
			}
			list := ipc.ProcessList{Time: time.Now(), NumCPU: runtime.NumCPU()}
			procMu.Lock()
			defer procMu.Unlock()
			for _, svc := range col.Services() {
				if !want[svc.Name] || svc.Cgroup == "" {
					continue
				}
				procs, err := procReader.ReadProcesses(svc.Cgroup)
				if err != nil {
					continue
				}
				list.Units = append(list.Units, svc.Name)
				list.Processes = append(list.Processes, procs...)
				if len(list.Processes) >= metrics.MaxProcesses {
					list.Processes = list.Processes[:metrics.MaxProcesses]
					break
				}
			}
			if len(list.Units) == 0 {
				return nil, errors.New("la unidad ya no está en ejecución")
			}
			return list, nil
		}})

	// Estado completo para la vista "watch" de la CLI.
	srv.Register(daemon.Command{Name: "snapshot", Hidden: true,
		Data: func([]string) (any, error) {
			s, ok := col.Latest()
			if !ok {
				return nil, errNoSample
			}
			return ipc.Snapshot{Sample: s, Services: col.Services(), Events: store.Recent(5), Interval: col.Interval()}, nil
		}})
}

func status(col *collector.Collector, store *events.Store) (string, error) {
	s, ok := col.Latest()
	if !ok {
		return "", errNoSample
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Muestra:  %s\n", s.Time.Format("15:04:05"))
	fmt.Fprintf(&b, "CPU:      %5.1f%%   carga %.2f %.2f %.2f\n", s.CPUPercent, s.Load1, s.Load5, s.Load15)
	fmt.Fprintf(&b, "Memoria:  %5.1f%%   %d MiB disponibles de %d MiB\n",
		s.MemUsedPercent, s.MemAvailableKB/1024, s.MemTotalKB/1024)
	fmt.Fprintf(&b, "Swap:     %5.1f%%\n", s.SwapUsedPercent)
	if s.PSI.Available {
		fmt.Fprintf(&b, "Presión:  cpu %.1f%%  memoria %.1f%%  E/S %.1f%%  (PSI some avg10)\n",
			s.PSI.CPU.Avg10, s.PSI.Memory.Avg10, s.PSI.IO.Avg10)
	} else {
		b.WriteString("Presión:  PSI no disponible en este kernel\n")
	}

	recent := store.Recent(0)
	cutoff := time.Now().Add(-time.Hour)
	var lastHour int
	for _, e := range recent {
		if e.Time.After(cutoff) && e.Kind != events.PeakEnd {
			lastHour++
		}
	}
	fmt.Fprintf(&b, "Incidencias en la última hora: %d", lastHour)
	if lastHour > 0 {
		b.WriteString(` (usa "events")`)
	}
	return b.String(), nil
}

// historyCmd: history [n] — últimas n muestras (por defecto 20).
func historyCmd(col *collector.Collector, args []string) (string, error) {
	n, err := countArg(args, 20)
	if err != nil {
		return "", err
	}
	samples := col.History(n)
	if len(samples) == 0 {
		return "", errNoSample
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "HORA\tCPU%\tMEM%\tSWAP%\tCARGA1\tPSI-CPU\tPSI-MEM\tPSI-IO\t")
	for _, s := range samples {
		fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%.1f\t%.2f\t%.1f\t%.1f\t%.1f\t\n",
			s.Time.Format("15:04:05"), s.CPUPercent, s.MemUsedPercent, s.SwapUsedPercent,
			s.Load1, s.PSI.CPU.Avg10, s.PSI.Memory.Avg10, s.PSI.IO.Avg10)
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n"), nil
}

// kindFilters relaciona los filtros de "services" con los tipos de unidad.
var kindFilters = map[string]string{
	"apps":         metrics.KindApp,
	"servicios":    metrics.KindService,
	"contenedores": metrics.KindContainer,
	"sesiones":     metrics.KindSession,
}

// servicesCmd: services [cpu|mem] [apps|servicios|contenedores] [n] — unidades
// ordenadas por consumo, opcionalmente solo de un tipo. Los argumentos pueden
// ir en cualquier orden.
func servicesCmd(col *collector.Collector, args []string) (string, error) {
	sortBy, kind, n := "cpu", "", 15
	for _, a := range args {
		if a == "cpu" || a == "mem" {
			sortBy = a
		} else if k, ok := kindFilters[a]; ok {
			kind = k
		} else if v, err := countArg([]string{a}, 0); err == nil {
			n = v
		} else {
			return "", fmt.Errorf("argumento desconocido %q (uso: services [cpu|mem] [apps|servicios|contenedores|sesiones] [n])", a)
		}
	}

	var svcs []collector.ServiceSample
	for _, s := range collector.Grouped(col.Services()) {
		if kind == "" || s.Kind == kind {
			svcs = append(svcs, s)
		}
	}
	if len(svcs) == 0 {
		return "", errors.New("no se encontraron unidades de ese tipo (revisa services.slices en la configuración)")
	}
	sort.SliceStable(svcs, func(i, j int) bool {
		if sortBy == "mem" {
			return svcs[i].MemBytes > svcs[j].MemBytes
		}
		return svcs[i].CPUPercent > svcs[j].CPUPercent
	})
	if len(svcs) > n {
		svcs = svcs[:n]
	}

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NOMBRE\tTIPO\tCPU%\tMEMORIA\tLÍMITE\tOOM KILLS")
	for _, s := range svcs {
		limit := "-"
		if s.MemMax > 0 {
			limit = mib(s.MemMax)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f\t%s\t%s\t%d\n", s.Label(), s.Kind, s.CPUPercent, mib(s.MemBytes), limit, s.OOMKills)
	}
	tw.Flush()
	b.WriteString("CPU% es sobre la capacidad total de la máquina.")
	return b.String(), nil
}

// eventsCmd: events [n] — últimas n incidencias (por defecto 20).
func eventsCmd(store *events.Store, args []string) (string, error) {
	n, err := countArg(args, 20)
	if err != nil {
		return "", err
	}
	evs := store.Recent(n)
	if len(evs) == 0 {
		return "Sin incidencias registradas.", nil
	}
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "#%-4d %s  %-8s %s\n", e.ID, e.Time.Format("2006-01-02 15:04:05"), severityLabel(e.Severity), e.Message)
	}
	b.WriteString(`Detalle con "event <id>".`)
	return b.String(), nil
}

// eventCmd: event <id> — detalle de una incidencia con el contexto del sistema.
func eventCmd(store *events.Store, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("uso: event <id>")
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil {
		return "", fmt.Errorf("id inválido %q", args[0])
	}
	for _, e := range store.Recent(0) {
		if e.ID != id {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Incidencia #%d — %s\n", e.ID, e.Message)
		fmt.Fprintf(&b, "Fecha:     %s\n", e.Time.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "Tipo:      %s (%s)\n", e.Kind, e.Severity)
		if e.Service != "" {
			fmt.Fprintf(&b, "Servicio:  %s\n", e.Service)
		}
		if e.Duration != "" {
			fmt.Fprintf(&b, "Duración:  %s\n", e.Duration)
		}
		if c := e.Context; c != nil {
			fmt.Fprintf(&b, "Sistema:   CPU %.1f%%, memoria %.1f%%, carga %.2f, PSI cpu/mem/io %.1f/%.1f/%.1f\n",
				c.CPUPercent, c.MemUsedPercent, c.Load1, c.PSICPU, c.PSIMemory, c.PSIIO)
			if len(c.TopCPU) > 0 {
				b.WriteString("Más CPU:  ")
				for _, s := range c.TopCPU {
					fmt.Fprintf(&b, " %s (%.1f%%)", s.Label(), s.CPUPercent)
				}
				b.WriteString("\n")
			}
			if len(c.TopMemory) > 0 {
				b.WriteString("Más memoria:")
				for _, s := range c.TopMemory {
					fmt.Fprintf(&b, " %s (%s)", s.Label(), mib(s.MemBytes))
				}
				b.WriteString("\n")
			}
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return "", fmt.Errorf("no hay ninguna incidencia #%d en memoria", id)
}

func countArg(args []string, def int) (int, error) {
	if len(args) == 0 {
		return def, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("número inválido %q", args[0])
	}
	return n, nil
}

func severityLabel(s events.Severity) string {
	switch s {
	case events.Critical:
		return "CRÍTICO"
	case events.Warning:
		return "AVISO"
	default:
		return "INFO"
	}
}

func mib(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1024*1024)) }
