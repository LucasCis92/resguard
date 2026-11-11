// resguardd es el demonio que vigila los recursos del sistema y de los servicios.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/daemon"
	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
	"github.com/LucasCis92/resguard/internal/units"
	"github.com/LucasCis92/resguard/internal/version"
)

func main() {
	th := events.DefaultThresholds
	var (
		socketPath = flag.String("socket", ipc.DefaultSocketPath, "ruta del socket Unix")
		group      = flag.String("group", "", "grupo propietario del socket (sus miembros pueden usar la CLI)")
		interval   = flag.Duration("interval", 2*time.Second, "periodo de muestreo")
		history    = flag.Duration("history", time.Hour, "tiempo de historial a conservar en memoria")
		slices     = flag.String("slices", "system.slice", "slices de cgroups donde buscar servicios, separados por comas")
		eventsFile = flag.String("events-file", "", "fichero JSON Lines donde guardar las incidencias (vacío: solo en memoria y log)")
		bus        = flag.String("systemd", "system", "instancia de systemd a vigilar: system, user u off")
	)
	flag.Float64Var(&th.CPU, "cpu-threshold", th.CPU, "% de CPU a partir del cual hay un pico (0 desactiva)")
	flag.Float64Var(&th.Memory, "mem-threshold", th.Memory, "% de memoria a partir del cual hay un pico (0 desactiva)")
	flag.Float64Var(&th.PSICPU, "psi-cpu-threshold", th.PSICPU, "PSI cpu some avg10 a partir del cual hay un pico (0 desactiva)")
	flag.Float64Var(&th.PSIMemory, "psi-mem-threshold", th.PSIMemory, "PSI memory some avg10 a partir del cual hay un pico (0 desactiva)")
	flag.Float64Var(&th.PSIIO, "psi-io-threshold", th.PSIIO, "PSI io some avg10 a partir del cual hay un pico (0 desactiva)")
	flag.IntVar(&th.Sustain, "sustain", th.Sustain, "muestras seguidas por encima del umbral para registrar un pico")
	flag.Parse()

	// Bajo systemd, stderr va al journal.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *socketPath, *group, *interval, *history, splitList(*slices), *eventsFile, units.Bus(*bus), th); err != nil {
		log.Error("el demonio terminó con error", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, socketPath, group string, interval, history time.Duration,
	slices []string, eventsFile string, bus units.Bus, th events.Thresholds) error {

	switch bus {
	case units.SystemBus, units.UserBus, "off":
	default:
		return fmt.Errorf("valor inválido para -systemd: %q (system, user u off)", bus)
	}
	if interval < 100*time.Millisecond {
		return fmt.Errorf("-interval demasiado corto: %s", interval)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := events.NewStore(eventsFile, 1000, log)
	if err != nil {
		return err
	}
	defer store.Close()

	col := collector.New(collector.Config{
		Interval: interval,
		History:  int(history / interval),
		Slices:   slices,
	}, metrics.NewReader(), log)
	col.Observe(events.NewDetector(store, th).Observe)

	srv := daemon.NewServer(socketPath, log)
	srv.SetSocketGroup(group)
	registerCommands(srv, col, store)

	log.Info("iniciando resguardd", "version", version.Version, "interval", interval,
		"slices", slices, "events_file", eventsFile, "systemd", bus)

	go col.Run(ctx)
	if bus != "off" {
		w := units.NewWatcher(bus, store, log, func() *events.Context {
			s, ok := col.Latest()
			if !ok {
				return nil
			}
			return events.NewContext(s, col.Services(), 3)
		})
		go w.Run(ctx)
	}

	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info("resguardd detenido")
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
