// resguardd es el demonio que vigila los recursos del sistema y de los servicios.
//
// Los umbrales, intervalos y servicios vigilados se leen de un archivo YAML
// (por defecto /etc/resguard/config.yaml) que se recarga con SIGHUP
// (systemctl reload resguard). Las opciones de línea de órdenes equivalentes,
// si se indican, tienen prioridad sobre el archivo; están pensadas para el
// desarrollo.
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
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
	"github.com/LucasCis92/resguard/internal/units"
	"github.com/LucasCis92/resguard/internal/version"
)

// options son los ajustes de despliegue, que solo se cambian reiniciando.
type options struct {
	socket, group, eventsFile string
	bus                       units.Bus
}

func main() {
	var (
		opts       options
		bus        string
		configPath = flag.String("config", config.DefaultPath, "archivo de configuración")
		checkOnly  = flag.Bool("check-config", false, "valida la configuración, la muestra y sale")
	)
	flag.StringVar(&opts.socket, "socket", ipc.DefaultSocketPath, "ruta del socket Unix")
	flag.StringVar(&opts.group, "group", "", "grupo propietario del socket (sus miembros pueden usar la CLI)")
	flag.StringVar(&opts.eventsFile, "events-file", "", "fichero JSON Lines donde guardar las incidencias (vacío: solo en memoria y log)")
	flag.StringVar(&bus, "systemd", "system", "instancia de systemd a vigilar: system, user u off")

	// Equivalentes a claves del archivo de configuración; si se indican, mandan sobre él.
	def := config.Default()
	interval := flag.Duration("interval", def.Interval, "periodo de muestreo (sobrescribe interval)")
	history := flag.Duration("history", def.History, "historial a conservar en memoria (sobrescribe history)")
	slices := flag.String("slices", strings.Join(def.Services.Slices, ","), "slices de cgroups, separados por comas (sobrescribe services.slices)")
	th := def.Thresholds
	flag.Float64Var(&th.CPU, "cpu-threshold", th.CPU, "umbral de CPU en % (sobrescribe thresholds.cpu)")
	flag.Float64Var(&th.Memory, "mem-threshold", th.Memory, "umbral de memoria en % (sobrescribe thresholds.memory)")
	flag.Float64Var(&th.PSICPU, "psi-cpu-threshold", th.PSICPU, "umbral de PSI cpu (sobrescribe thresholds.psi_cpu)")
	flag.Float64Var(&th.PSIMemory, "psi-mem-threshold", th.PSIMemory, "umbral de PSI memory (sobrescribe thresholds.psi_memory)")
	flag.Float64Var(&th.PSIIO, "psi-io-threshold", th.PSIIO, "umbral de PSI io (sobrescribe thresholds.psi_io)")
	flag.IntVar(&th.Sustain, "sustain", th.Sustain, "muestras seguidas sobre el umbral (sobrescribe thresholds.sustain)")
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

	// Si se pidió un archivo concreto con -config, su ausencia es un error;
	// la ruta por defecto, en cambio, es opcional.
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
			fmt.Fprintln(os.Stderr, "Configuración inválida:", err)
			os.Exit(1)
		}
		fmt.Println(lc.describe())
		return
	}

	// Bajo systemd, stderr va al journal.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, opts, loader); err != nil {
		log.Error("el demonio terminó con error", "err", err)
		os.Exit(1)
	}
}

// loadedConfig es una configuración junto con su origen.
type loadedConfig struct {
	config.Config
	path   string
	found  bool
	loaded time.Time
}

func (lc loadedConfig) describe() string {
	var header string
	if lc.found {
		header = fmt.Sprintf("# Archivo: %s (cargado el %s)", lc.path, lc.loaded.Format("2006-01-02 15:04:05"))
	} else {
		header = fmt.Sprintf("# %s no existe: se usan los valores por defecto", lc.path)
	}
	return header + "\n" + lc.YAML()
}

// current guarda la configuración en uso; la leen varias goroutines.
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
		return fmt.Errorf("valor inválido para -systemd: %q (system, user u off)", opts.bus)
	}

	lc, err := loader()
	if err != nil {
		return fmt.Errorf("configuración inválida: %w", err)
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
		log.Info("configuración cargada", "path", lc.path)
	} else {
		log.Info("sin archivo de configuración; se usan los valores por defecto", "path", lc.path)
	}
	log.Info("iniciando resguardd", "version", version.Version, "interval", lc.Interval,
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
	log.Info("resguardd detenido")
	return nil
}

// reloadOnSIGHUP relee la configuración con cada SIGHUP. Si la nueva no es
// válida, se registra el error y se mantiene la anterior.
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
				log.Error("la configuración nueva no es válida; se mantiene la anterior", "err", err)
				continue
			}
			apply(lc)
			log.Info("configuración recargada", "path", lc.path, "found", lc.found,
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
