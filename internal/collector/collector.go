// Package collector toma muestras periódicas del sistema y de los servicios,
// calcula los valores derivados (porcentajes de CPU) y guarda un historial.
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

// Sample es una muestra ya procesada del estado del sistema.
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

// ServiceSample es el uso de recursos de un servicio en una muestra.
type ServiceSample struct {
	Name    string `json:"name"`              // unidad de systemd
	Display string `json:"display,omitempty"` // nombre legible (Chrome, laravel_app…)
	Kind    string `json:"kind,omitempty"`    // servicio, app, contenedor, sesión u otro
	Cgroup  string `json:"-"`                 // ruta del cgroup; no se envía a la CLI
	// Units son las unidades que forman el grupo, en los resultados de Grouped.
	Units      []string `json:"units,omitempty"`
	CPUPercent float64  `json:"cpu_percent"` // sobre la capacidad total de la máquina
	MemBytes   uint64   `json:"mem_bytes"`
	MemMax     uint64   `json:"mem_max,omitempty"` // 0 = sin límite
	OOMKills   uint64   `json:"oom_kills"`         // acumulado desde que existe el cgroup
}

// Observer recibe cada muestra nueva. Se llama de forma síncrona desde el
// bucle del recolector, así que debe volver rápido.
type Observer func(Sample, []ServiceSample)

// Config ajusta el recolector.
type Config struct {
	Interval time.Duration // periodo de muestreo
	History  int           // número de muestras a conservar
	Slices   []string      // slices de cgroups donde buscar servicios
}

// Collector muestrea el sistema periódicamente.
type Collector struct {
	reader    *metrics.Reader
	log       *slog.Logger
	observers []Observer
	numCPU    float64

	mu       sync.RWMutex
	cfg      Config
	reset    chan struct{} // avisa a Run de que cambió el intervalo
	history  *Ring[Sample]
	services []ServiceSample // solo los de la última muestra

	// Lecturas en bruto anteriores, para calcular deltas.
	prevSys metrics.System
	prevSvc map[string]metrics.Service
	hasPrev bool
}

// New crea un recolector.
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

// Configure aplica una configuración nueva mientras el recolector funciona.
// El historial conserva las muestras más recientes que quepan en el nuevo tamaño.
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

// Interval devuelve el periodo de muestreo actual.
func (c *Collector) Interval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Interval
}

// Observe registra un observador. Debe llamarse antes de Run.
func (c *Collector) Observe(o Observer) { c.observers = append(c.observers, o) }

// Run muestrea hasta que ctx se cancela.
func (c *Collector) Run(ctx context.Context) {
	// La primera lectura solo sirve de referencia para los deltas.
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

// Tick toma una muestra. Es pública para poder dirigir el recolector en los tests.
func (c *Collector) Tick() {
	sys, err := c.reader.ReadSystem()
	if err != nil {
		c.log.Error("leyendo métricas del sistema", "err", err)
		return
	}
	c.mu.RLock()
	slices := c.cfg.Slices
	c.mu.RUnlock()
	raw, err := c.reader.ReadServices(slices)
	if err != nil {
		c.log.Warn("leyendo métricas de servicios", "err", err)
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
				// Unidad nueva: como se recorren todas en cada muestra, se creó
				// después de la anterior y todo su consumo es de este intervalo.
				// (Si supera lo posible en el intervalo, es una unidad antigua que
				// acaba de entrar en la vigilancia, p. ej. al cambiar los slices.)
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

// Latest devuelve la última muestra y false si todavía no hay ninguna.
func (c *Collector) Latest() (Sample, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	last := c.history.Last(1)
	if len(last) == 0 {
		return Sample{}, false
	}
	return last[0], true
}

// History devuelve hasta n muestras, de la más antigua a la más reciente.
func (c *Collector) History(n int) []Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.history.Last(n)
}

// Services devuelve el uso de los servicios en la última muestra.
func (c *Collector) Services() []ServiceSample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]ServiceSample(nil), c.services...)
}

// Label devuelve el nombre legible, o el de la unidad si no hay otro.
func (s ServiceSample) Label() string {
	if s.Display != "" {
		return s.Display
	}
	return s.Name
}

// Grouped une las unidades del mismo tipo y nombre legible (p. ej. dos
// unidades de una misma aplicación), sumando su consumo. El orden se conserva.
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
			o.MemMax = 0 // si alguna no tiene límite, el conjunto tampoco
		}
	}
	return out
}
