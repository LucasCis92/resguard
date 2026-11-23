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
	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
	"github.com/LucasCis92/resguard/internal/version"
)

func errNoSample(p i18n.Printer) error {
	return errors.New(p.T("no samples yet; wait a couple of seconds"))
}

func registerCommands(srv *daemon.Server, col *collector.Collector, store *events.Store, cur *current) {
	srv.Register(daemon.Command{Name: "ping", Description: i18n.M("check that the daemon answers"),
		Text: func(daemon.Call) (string, error) { return "pong", nil }})
	srv.Register(daemon.Command{Name: "version", Description: i18n.M("daemon version"),
		Text: func(daemon.Call) (string, error) { return version.Version, nil }})
	srv.Register(daemon.Command{Name: "status", Description: i18n.M("summary of the system state"),
		Text: func(c daemon.Call) (string, error) { return status(col, store, c.P) }})
	srv.Register(daemon.Command{Name: "history", Usage: i18n.M("history [n]"),
		Description: i18n.M("last n samples (20 by default)"),
		Text:        func(c daemon.Call) (string, error) { return historyCmd(col, c) }})
	srv.Register(daemon.Command{Name: "services", Usage: i18n.M("services [cpu|mem] [apps|services|containers|sessions] [n]"),
		Description: i18n.M("services, applications and containers using the most resources"),
		Text:        func(c daemon.Call) (string, error) { return servicesCmd(col, c) }})
	srv.Register(daemon.Command{Name: "events", Usage: i18n.M("events [n]"),
		Description: i18n.M("last n incidents (20 by default)"),
		Text:        func(c daemon.Call) (string, error) { return eventsCmd(store, c) }})
	srv.Register(daemon.Command{Name: "event", Usage: i18n.M("event <id>"),
		Description: i18n.M("details of an incident"),
		Text:        func(c daemon.Call) (string, error) { return eventCmd(store, c) }})
	srv.Register(daemon.Command{Name: "config", Description: i18n.M("configuration in use and where it was loaded from"),
		Text: func(c daemon.Call) (string, error) { return cur.get().describe(c.P), nil }})

	// Processes of some units, for the process view of "watch". Only unit
	// names the collector knows are accepted: the client never chooses the
	// cgroup path.
	procReader := metrics.NewReader() // its own: the collector's is not concurrent
	var procMu sync.Mutex
	srv.Register(daemon.Command{Name: "processes", Hidden: true,
		Data: func(c daemon.Call) (any, error) {
			if len(c.Args) == 0 || len(c.Args) > 20 {
				return nil, errors.New(c.P.T("usage: processes <unit>..."))
			}
			want := map[string]bool{}
			for _, a := range c.Args {
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
				return nil, errors.New(c.P.T("the unit is no longer running"))
			}
			return list, nil
		}})

	// Full state for the CLI's "watch" view.
	srv.Register(daemon.Command{Name: "snapshot", Hidden: true,
		Data: func(c daemon.Call) (any, error) {
			s, ok := col.Latest()
			if !ok {
				return nil, errNoSample(c.P)
			}
			return ipc.Snapshot{Sample: s, Services: col.Services(), Events: store.Recent(5), Interval: col.Interval()}, nil
		}})
}

func status(col *collector.Collector, store *events.Store, p i18n.Printer) (string, error) {
	s, ok := col.Latest()
	if !ok {
		return "", errNoSample(p)
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\t%s\n", p.T("Sample:"), s.Time.Format("15:04:05"))
	fmt.Fprintf(tw, "%s\t%5.1f%%   %s\n", p.T("CPU:"), s.CPUPercent, p.T("load %.2f %.2f %.2f", s.Load1, s.Load5, s.Load15))
	fmt.Fprintf(tw, "%s\t%5.1f%%   %s\n", p.T("Memory:"), s.MemUsedPercent,
		p.T("%d MiB available of %d MiB", s.MemAvailableKB/1024, s.MemTotalKB/1024))
	fmt.Fprintf(tw, "%s\t%5.1f%%\n", p.T("Swap:"), s.SwapUsedPercent)
	if s.PSI.Available {
		fmt.Fprintf(tw, "%s\t%s\n", p.T("Pressure:"),
			p.T("cpu %.1f%%  memory %.1f%%  I/O %.1f%%  (PSI some avg10)", s.PSI.CPU.Avg10, s.PSI.Memory.Avg10, s.PSI.IO.Avg10))
	} else {
		fmt.Fprintf(tw, "%s\t%s\n", p.T("Pressure:"), p.T("PSI is not available on this kernel"))
	}
	tw.Flush()

	cutoff := time.Now().Add(-time.Hour)
	var lastHour int
	for _, e := range store.Recent(0) {
		if e.Time.After(cutoff) && e.Kind != events.PeakEnd {
			lastHour++
		}
	}
	b.WriteString(p.T("Incidents in the last hour: %d", lastHour))
	if lastHour > 0 {
		b.WriteString(" " + p.T(`(see "events")`))
	}
	return b.String(), nil
}

// historyCmd: history [n]: the last n samples (20 by default).
func historyCmd(col *collector.Collector, c daemon.Call) (string, error) {
	n, err := countArg(c.Args, 20, c.P)
	if err != nil {
		return "", err
	}
	samples := col.History(n)
	if len(samples) == 0 {
		return "", errNoSample(c.P)
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%s\tCPU%%\tMEM%%\tSWAP%%\t%s\tPSI-CPU\tPSI-MEM\tPSI-IO\t\n", c.P.T("TIME"), c.P.T("LOAD1"))
	for _, s := range samples {
		fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%.1f\t%.2f\t%.1f\t%.1f\t%.1f\t\n",
			s.Time.Format("15:04:05"), s.CPUPercent, s.MemUsedPercent, s.SwapUsedPercent,
			s.Load1, s.PSI.CPU.Avg10, s.PSI.Memory.Avg10, s.PSI.IO.Avg10)
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n"), nil
}

// kindFilters maps the filters of "services" (English and Spanish) to unit kinds.
var kindFilters = map[string]string{
	"apps":     metrics.KindApp,
	"services": metrics.KindService, "servicios": metrics.KindService,
	"containers": metrics.KindContainer, "contenedores": metrics.KindContainer,
	"sessions": metrics.KindSession, "sesiones": metrics.KindSession,
}

// servicesCmd: services [cpu|mem] [apps|services|containers|sessions] [n]:
// units sorted by usage, optionally of one kind only. Arguments may come in
// any order.
func servicesCmd(col *collector.Collector, c daemon.Call) (string, error) {
	p := c.P
	sortBy, kind, n := "cpu", "", 15
	for _, a := range c.Args {
		if a == "cpu" || a == "mem" {
			sortBy = a
		} else if k, ok := kindFilters[a]; ok {
			kind = k
		} else if v, err := countArg([]string{a}, 0, p); err == nil {
			n = v
		} else {
			return "", errors.New(p.T("unknown argument %q (usage: services [cpu|mem] [apps|services|containers|sessions] [n])", a))
		}
	}

	var svcs []collector.ServiceSample
	for _, s := range collector.Grouped(col.Services()) {
		if kind == "" || s.Kind == kind {
			svcs = append(svcs, s)
		}
	}
	if len(svcs) == 0 {
		return "", errors.New(p.T("no units of that kind were found (check services.slices in the configuration)"))
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
	fmt.Fprintf(tw, "%s\t%s\tCPU%%\t%s\t%s\tOOM KILLS\n", p.T("NAME"), p.T("KIND"), p.T("MEMORY"), p.T("LIMIT"))
	for _, s := range svcs {
		limit := "-"
		if s.MemMax > 0 {
			limit = mib(s.MemMax)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f\t%s\t%s\t%d\n", s.Label(), events.KindLabel(s.Kind, p), s.CPUPercent, mib(s.MemBytes), limit, s.OOMKills)
	}
	tw.Flush()
	b.WriteString(p.T("CPU usage is relative to the whole machine's capacity."))
	return b.String(), nil
}

// eventsCmd: events [n]: the last n incidents (20 by default).
func eventsCmd(store *events.Store, c daemon.Call) (string, error) {
	n, err := countArg(c.Args, 20, c.P)
	if err != nil {
		return "", err
	}
	evs := store.Recent(n)
	if len(evs) == 0 {
		return c.P.T("No incidents recorded."), nil
	}
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "#%-4d %s  %-8s %s\n", e.ID, e.Time.Format("2006-01-02 15:04:05"),
			events.SeverityLabel(e.Severity, c.P), events.Describe(e, c.P))
	}
	b.WriteString(c.P.T(`Details with "event <id>".`))
	return b.String(), nil
}

// eventCmd: event <id>: details of an incident with the system context.
func eventCmd(store *events.Store, c daemon.Call) (string, error) {
	p := c.P
	if len(c.Args) != 1 {
		return "", errors.New(p.T("usage: event <id>"))
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(c.Args[0], "#"), 10, 64)
	if err != nil {
		return "", errors.New(p.T("invalid id %q", c.Args[0]))
	}
	for _, e := range store.Recent(0) {
		if e.ID != id {
			continue
		}
		var b strings.Builder
		b.WriteString(p.T("Incident #%d: %s", e.ID, events.Describe(e, p)) + "\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\n", p.T("Date:"), e.Time.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(tw, "%s\t%s (%s)\n", p.T("Type:"), e.Kind, events.SeverityLabel(e.Severity, p))
		if e.Service != "" {
			fmt.Fprintf(tw, "%s\t%s\n", p.T("Unit:"), e.Service)
		}
		if e.Duration != "" {
			fmt.Fprintf(tw, "%s\t%s\n", p.T("Duration:"), e.Duration)
		}
		if ctx := e.Context; ctx != nil {
			fmt.Fprintf(tw, "%s\t%s\n", p.T("System:"), p.T("CPU %.1f%%, memory %.1f%%, load %.2f, PSI cpu/mem/io %.1f/%.1f/%.1f",
				ctx.CPUPercent, ctx.MemUsedPercent, ctx.Load1, ctx.PSICPU, ctx.PSIMemory, ctx.PSIIO))
			if len(ctx.TopCPU) > 0 {
				var parts []string
				for _, s := range ctx.TopCPU {
					parts = append(parts, fmt.Sprintf("%s (%.1f%%)", s.Label(), s.CPUPercent))
				}
				fmt.Fprintf(tw, "%s\t%s\n", p.T("Top CPU:"), strings.Join(parts, "  "))
			}
			if len(ctx.TopMemory) > 0 {
				var parts []string
				for _, s := range ctx.TopMemory {
					parts = append(parts, fmt.Sprintf("%s (%s)", s.Label(), mib(s.MemBytes)))
				}
				fmt.Fprintf(tw, "%s\t%s\n", p.T("Top memory:"), strings.Join(parts, "  "))
			}
		}
		tw.Flush()
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return "", errors.New(p.T("there is no incident #%d in memory", id))
}

func countArg(args []string, def int, p i18n.Printer) (int, error) {
	if len(args) == 0 {
		return def, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, errors.New(p.T("invalid number %q", args[0]))
	}
	return n, nil
}

func mib(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1024*1024)) }
