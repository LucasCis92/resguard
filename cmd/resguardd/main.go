// resguardd is the daemon that monitors the resources of the system and its
// services.
//
// Thresholds, intervals and monitored units are read from a YAML file (by
// default /etc/resguard/config.yaml) that is reloaded on SIGHUP (systemctl
// reload resguard). The equivalent command-line flags, when given, take
// precedence over the file; they are meant for development.
//
// The daemon's own output (logs, -check-config) is always English; the text
// it sends to resguardctl follows each client's language.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/config"
	"github.com/LucasCis92/resguard/internal/daemon"
	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
	"github.com/LucasCis92/resguard/internal/units"
	"github.com/LucasCis92/resguard/internal/version"
)

// options are deployment settings, which only change on restart.
type options struct {
	socket, group, eventsFile string
	bus                       units.Bus
}

func main() {
	var (
		opts       options
		bus        string
		configPath = flag.String("config", config.DefaultPath, "configuration file")
		checkOnly  = flag.Bool("check-config", false, "validate the configuration, print it and exit")
	)
	flag.StringVar(&opts.socket, "socket", ipc.DefaultSocketPath, "Unix socket path")
	flag.StringVar(&opts.group, "group", "", "group owning the socket (its members can use the CLI)")
	flag.StringVar(&opts.eventsFile, "events-file", "", "JSON Lines file to append incidents to (empty: memory and log only)")
	flag.StringVar(&bus, "systemd", "system", "systemd instance to watch: system, user or off")

	// Equivalent to keys of the configuration file; when given, they win over it.
	def := config.Default()
	interval := flag.Duration("interval", def.Interval, "sampling period (overrides interval)")
	history := flag.Duration("history", def.History, "history to keep in memory (overrides history)")
	slices := flag.String("slices", strings.Join(def.Services.Slices, ","), "comma-separated cgroup slices (overrides services.slices)")
	th := def.Thresholds
	flag.Float64Var(&th.CPU, "cpu-threshold", th.CPU, "CPU threshold in % (overrides thresholds.cpu)")
	flag.Float64Var(&th.Memory, "mem-threshold", th.Memory, "memory threshold in % (overrides thresholds.memory)")
	flag.Float64Var(&th.PSICPU, "psi-cpu-threshold", th.PSICPU, "PSI cpu threshold (overrides thresholds.psi_cpu)")
	flag.Float64Var(&th.PSIMemory, "psi-mem-threshold", th.PSIMemory, "PSI memory threshold (overrides thresholds.psi_memory)")
	flag.Float64Var(&th.PSIIO, "psi-io-threshold", th.PSIIO, "PSI io threshold (overrides thresholds.psi_io)")
	flag.IntVar(&th.Sustain, "sustain", th.Sustain, "consecutive samples above the threshold (overrides thresholds.sustain)")
	flag.Parse()
	opts.bus = units.Bus(bus)

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	override := func(c *config.Config) {
		set := func(name string, apply func()) {
			if explicit[name] {
				apply()
			}
		}
		set("interval", func() { c.Interval = *interval })
		set("history", func() { c.History = *history })
		set("slices", func() { c.Services.Slices = splitList(*slices) })
		set("cpu-threshold", func() { c.Thresholds.CPU = th.CPU })
		set("mem-threshold", func() { c.Thresholds.Memory = th.Memory })
		set("psi-cpu-threshold", func() { c.Thresholds.PSICPU = th.PSICPU })
		set("psi-mem-threshold", func() { c.Thresholds.PSIMemory = th.PSIMemory })
		set("psi-io-threshold", func() { c.Thresholds.PSIIO = th.PSIIO })
		set("sustain", func() { c.Thresholds.Sustain = th.Sustain })
	}

	// A file requested explicitly with -config must exist; the default path
	// is optional.
	loader := func() (loadedConfig, error) {
		cfg, found, err := config.Load(*configPath, !explicit["config"])
		if err != nil {
			return loadedConfig{}, err
		}
		override(&cfg)
		if err := cfg.Validate(); err != nil {
			return loadedConfig{}, err
		}
		return loadedConfig{Config: cfg, path: *configPath, found: found, loaded: time.Now()}, nil
	}

	if *checkOnly {
		lc, err := loader()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Invalid configuration:", err)
			os.Exit(1)
		}
		fmt.Println(lc.describe(i18n.New(i18n.English)))
		return
	}

	// Under systemd, stderr goes to the journal, which already timestamps every line.
	var hopts slog.HandlerOptions
	if stderrIsJournal() {
		hopts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &hopts))
	if err := run(log, opts, loader); err != nil {
		log.Error("the daemon exited with an error", "err", err)
		os.Exit(1)
	}
}

// stderrIsJournal reports whether stderr is connected to the journal.
// JOURNAL_STREAM is also inherited by processes started from a service (e.g. a
// VS Code terminal), so it is compared with stderr's actual device and inode,
// as systemd recommends.
func stderrIsJournal() bool {
	var dev, ino uint64
	if _, err := fmt.Sscanf(os.Getenv("JOURNAL_STREAM"), "%d:%d", &dev, &ino); err != nil {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stderr.Fd()), &st); err != nil {
		return false
	}
	return uint64(st.Dev) == dev && uint64(st.Ino) == ino
}

// loadedConfig is a configuration together with its origin.
type loadedConfig struct {
	config.Config
	path   string
	found  bool
	loaded time.Time
}

func (lc loadedConfig) describe(p i18n.Printer) string {
	var header string
	if lc.found {
		header = p.T("# File: %s (loaded on %s)", lc.path, lc.loaded.Format("2006-01-02 15:04:05"))
	} else {
		header = p.T("# %s does not exist: using the default values", lc.path)
	}
	return header + "\n" + lc.YAML()
}

// current holds the configuration in use; several goroutines read it.
type current struct {
	mu sync.RWMutex
	lc loadedConfig
}

func (c *current) get() loadedConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lc
}

func (c *current) set(lc loadedConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lc = lc
}

func run(log *slog.Logger, opts options, loader func() (loadedConfig, error)) error {
	switch opts.bus {
	case units.SystemBus, units.UserBus, "off":
	default:
		return fmt.Errorf("invalid value for -systemd: %q (system, user or off)", opts.bus)
	}

	lc, err := loader()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	cur := &current{lc: lc}
	ignored := func(service string) bool { return cur.get().Ignored(service) }

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := events.NewStore(opts.eventsFile, 1000, log)
	if err != nil {
		return err
	}
	defer store.Close()

	col := collector.New(collectorConfig(lc.Config), metrics.NewReader(), log)
	det := events.NewDetector(store, lc.Thresholds)
	det.SetIgnore(ignored)
	col.Observe(det.Observe)

	srv := daemon.NewServer(opts.socket, log)
	srv.SetSocketGroup(opts.group)
	registerCommands(srv, col, store, cur)

	if lc.found {
		log.Info("configuration loaded", "path", lc.path)
	} else {
		log.Info("no configuration file; using the default values", "path", lc.path)
	}
	log.Info("starting resguardd", "version", version.Version, "interval", lc.Interval,
		"slices", lc.Services.Slices, "events_file", opts.eventsFile, "systemd", opts.bus)

	go col.Run(ctx)
	go reloadOnSIGHUP(ctx, log, loader, func(lc loadedConfig) {
		col.Configure(collectorConfig(lc.Config))
		det.SetThresholds(lc.Thresholds)
		cur.set(lc)
	})
	if opts.bus != "off" {
		w := units.NewWatcher(opts.bus, store, log, func() *events.Context {
			s, ok := col.Latest()
			if !ok {
				return nil
			}
			return events.NewContext(s, col.Services(), 3)
		}, ignored)
		go w.Run(ctx)
	}

	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info("resguardd stopped")
	return nil
}

// reloadOnSIGHUP reloads the configuration on every SIGHUP. If the new one is
// invalid, the error is logged and the previous one is kept.
func reloadOnSIGHUP(ctx context.Context, log *slog.Logger, loader func() (loadedConfig, error), apply func(loadedConfig)) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			lc, err := loader()
			if err != nil {
				log.Error("the new configuration is invalid; keeping the previous one", "err", err)
				continue
			}
			apply(lc)
			log.Info("configuration reloaded", "path", lc.path, "found", lc.found,
				"interval", lc.Interval, "history", lc.History, "thresholds", fmt.Sprintf("%+v", lc.Thresholds),
				"slices", lc.Services.Slices, "ignore", lc.Services.Ignore)
		}
	}
}

func collectorConfig(c config.Config) collector.Config {
	return collector.Config{Interval: c.Interval, History: int(c.History / c.Interval), Slices: c.Services.Slices}
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
