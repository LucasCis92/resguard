// Package collector samples the system and its units periodically, computes
// derived values (CPU percentages) and keeps a history.
package collector

import (
	"context"
	"log/slog"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/LucasCis92/resguard/internal/metrics"
)

// Sample is a processed snapshot of the system state.
type Sample struct {
	Time            time.Time   `json:"time"`
	CPUPercent      float64     `json:"cpu_percent"`
	Load1           float64     `json:"load1"`
	Load5           float64     `json:"load5"`
	Load15          float64     `json:"load15"`
	MemUsedPercent  float64     `json:"mem_used_percent"`
	MemTotalKB      uint64      `json:"mem_total_kb"`
	MemAvailableKB  uint64      `json:"mem_available_kb"`
	SwapUsedPercent float64     `json:"swap_used_percent"`
	PSI             metrics.PSI `json:"psi"`
}

// ServiceSample is a unit's resource usage in one sample.
type ServiceSample struct {
	Name    string `json:"name"`              // systemd unit
	Display string `json:"display,omitempty"` // readable name (Chrome, laravel_app…)
	Kind    string `json:"kind,omitempty"`    // one of the metrics.Kind* constants
	Cgroup  string `json:"-"`                 // cgroup path; never sent to the CLI
	// Units are the units that make up a group, in the results of Grouped.
	Units      []string `json:"units,omitempty"`
	CPUPercent float64  `json:"cpu_percent"` // of the whole machine's capacity
	MemBytes   uint64   `json:"mem_bytes"`
	MemMax     uint64   `json:"mem_max,omitempty"` // 0 = no limit
	OOMKills   uint64   `json:"oom_kills"`         // cumulative since the cgroup was created
}

// Observer receives every new sample. It is called synchronously from the
// collector loop, so it must return quickly.
type Observer func(Sample, []ServiceSample)

// Config tunes the collector.
type Config struct {
	Interval time.Duration // sampling period
	History  int           // number of samples to keep
	Slices   []string      // cgroup slices to scan for units
}

// Collector samples the system periodically.
type Collector struct {
	reader    *metrics.Reader
	log       *slog.Logger
	observers []Observer
	numCPU    float64

	mu       sync.RWMutex
	cfg      Config
	reset    chan struct{} // tells Run that the interval changed
	history  *Ring[Sample]
	services []ServiceSample // only those of the latest sample

	// Previous raw readings, to compute deltas.
	prevSys metrics.System
	prevSvc map[string]metrics.Service
	hasPrev bool
}

// New creates a collector.
func New(cfg Config, reader *metrics.Reader, log *slog.Logger) *Collector {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	return &Collector{
		cfg:     cfg,
		reader:  reader,
		log:     log,
		numCPU:  float64(runtime.NumCPU()),
		reset:   make(chan struct{}, 1),
		history: NewRing[Sample](cfg.History),
	}
}

// Configure applies a new configuration while the collector runs. The history
// keeps the most recent samples that fit in the new size.
func (c *Collector) Configure(cfg Config) {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	c.mu.Lock()
	intervalChanged := cfg.Interval != c.cfg.Interval
	if cfg.History != c.cfg.History {
		c.history = c.history.Resized(cfg.History)
	}
	c.cfg = cfg
	c.mu.Unlock()

	if intervalChanged {
		select {
		case c.reset <- struct{}{}:
		default:
		}
	}
}

// Interval returns the current sampling period.
func (c *Collector) Interval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Interval
}

// Observe registers an observer. It must be called before Run.
func (c *Collector) Observe(o Observer) { c.observers = append(c.observers, o) }

// Run samples until ctx is canceled.
func (c *Collector) Run(ctx context.Context) {
	// The first reading is only a baseline for the deltas.
	c.Tick()
	t := time.NewTicker(c.Interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.reset:
			t.Reset(c.Interval())
		case <-t.C:
			c.Tick()
		}
	}
}

// Tick takes one sample. It is exported so tests can drive the collector.
func (c *Collector) Tick() {
	sys, err := c.reader.ReadSystem()
	if err != nil {
		c.log.Error("reading system metrics", "err", err)
		return
	}
	c.mu.RLock()
	slices := c.cfg.Slices
	c.mu.RUnlock()
	raw, err := c.reader.ReadServices(slices)
	if err != nil {
		c.log.Warn("reading unit metrics", "err", err)
	}

	cur := make(map[string]metrics.Service, len(raw))
	for _, s := range raw {
		cur[s.Name] = s
	}

	if !c.hasPrev {
		c.prevSys, c.prevSvc, c.hasPrev = sys, cur, true
		return
	}

	sample := Sample{
		Time:            sys.Time,
		CPUPercent:      metrics.CPUPercent(c.prevSys.CPU, sys.CPU),
		Load1:           sys.Load1,
		Load5:           sys.Load5,
		Load15:          sys.Load15,
		MemUsedPercent:  sys.MemUsedPercent(),
		MemTotalKB:      sys.MemTotalKB,
		MemAvailableKB:  sys.MemAvailableKB,
		SwapUsedPercent: sys.SwapUsedPercent(),
		PSI:             sys.PSI,
	}

	elapsedUsec := float64(sys.Time.Sub(c.prevSys.Time).Microseconds())
	services := make([]ServiceSample, 0, len(raw))
	for _, s := range raw {
		ss := ServiceSample{Name: s.Name, Display: s.Display, Kind: s.Kind, Cgroup: s.Cgroup, MemBytes: s.MemCurrent, MemMax: s.MemMax, OOMKills: s.OOMKills}
		if elapsedUsec > 0 {
			if p, ok := c.prevSvc[s.Name]; ok {
				if s.CPUUsageUsec >= p.CPUUsageUsec {
					ss.CPUPercent = 100 * float64(s.CPUUsageUsec-p.CPUUsageUsec) / elapsedUsec / c.numCPU
				}
			} else if float64(s.CPUUsageUsec) <= elapsedUsec*c.numCPU {
				// New unit: since every unit is scanned in every sample, it was
				// created after the previous one and all its usage belongs to
				// this interval. (If it exceeds what is possible in the interval,
				// it is an old unit that just entered monitoring, e.g. after the
				// slices changed.)
				ss.CPUPercent = 100 * float64(s.CPUUsageUsec) / elapsedUsec / c.numCPU
			}
		}
		services = append(services, ss)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	c.prevSys, c.prevSvc = sys, cur

	c.mu.Lock()
	c.history.Push(sample)
	c.services = services
	c.mu.Unlock()

	for _, o := range c.observers {
		o(sample, services)
	}
}

// Latest returns the latest sample, or false if there is none yet.
func (c *Collector) Latest() (Sample, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	last := c.history.Last(1)
	if len(last) == 0 {
		return Sample{}, false
	}
	return last[0], true
}

// History returns up to n samples, oldest first.
func (c *Collector) History(n int) []Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.history.Last(n)
}

// Services returns the units' usage in the latest sample.
func (c *Collector) Services() []ServiceSample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]ServiceSample(nil), c.services...)
}

// Label returns the readable name, or the unit name if there is none.
func (s ServiceSample) Label() string {
	if s.Display != "" {
		return s.Display
	}
	return s.Name
}

// Grouped merges units of the same kind and readable name (e.g. two units of
// the same application), adding up their usage. Order is preserved.
func Grouped(in []ServiceSample) []ServiceSample {
	idx := map[string]int{}
	var out []ServiceSample
	for _, s := range in {
		key := s.Kind + "\x00" + s.Label()
		i, ok := idx[key]
		if !ok {
			idx[key] = len(out)
			s.Units = []string{s.Name}
			out = append(out, s)
			continue
		}
		o := &out[i]
		o.Units = append(o.Units, s.Name)
		o.CPUPercent += s.CPUPercent
		o.MemBytes += s.MemBytes
		o.OOMKills += s.OOMKills
		if o.MemMax > 0 && s.MemMax > 0 {
			o.MemMax += s.MemMax
		} else {
			o.MemMax = 0 // if any member has no limit, neither does the group
		}
	}
	return out
}
