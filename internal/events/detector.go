package events

import (
	"fmt"
	"sync"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
)

// Thresholds son los umbrales a partir de los cuales se considera que hay un pico.
// Un valor 0 desactiva la regla correspondiente.
type Thresholds struct {
	CPU       float64 `yaml:"cpu"`        // % de uso de CPU
	Memory    float64 `yaml:"memory"`     // % de memoria usada
	PSICPU    float64 `yaml:"psi_cpu"`    // PSI cpu some avg10 (%)
	PSIMemory float64 `yaml:"psi_memory"` // PSI memory some avg10 (%)
	PSIIO     float64 `yaml:"psi_io"`     // PSI io some avg10 (%)
	Sustain   int     `yaml:"sustain"`    // muestras seguidas por encima del umbral para abrir un pico
}

// DefaultThresholds son valores razonables para un servidor de uso general.
var DefaultThresholds = Thresholds{
	CPU:       90,
	Memory:    90,
	PSICPU:    40,
	PSIMemory: 10,
	PSIIO:     30,
	Sustain:   2,
}

// Rule describe cuándo empieza y termina un pico de un recurso.
// Usa histéresis: el pico empieza al llegar a High y termina al bajar de Low,
// para no generar una avalancha de eventos cuando el valor oscila en el umbral.
type Rule struct {
	Resource string
	Label    string
	High     float64
	Low      float64
	Sustain  int
	NeedsPSI bool
	Value    func(collector.Sample) float64
}

// Rules construye las reglas a partir de los umbrales.
func (t Thresholds) Rules() []Rule {
	sustain := t.Sustain
	if sustain < 1 {
		sustain = 1
	}
	var rules []Rule
	add := func(resource, label string, high, margin float64, psi bool, value func(collector.Sample) float64) {
		if high <= 0 {
			return
		}
		rules = append(rules, Rule{
			Resource: resource, Label: label,
			High: high, Low: high - margin,
			Sustain: sustain, NeedsPSI: psi, Value: value,
		})
	}
	add("cpu", "uso de CPU", t.CPU, 10, false, func(s collector.Sample) float64 { return s.CPUPercent })
	add("memory", "uso de memoria", t.Memory, 5, false, func(s collector.Sample) float64 { return s.MemUsedPercent })
	add("psi_cpu", "presión de CPU", t.PSICPU, t.PSICPU/2, true, func(s collector.Sample) float64 { return s.PSI.CPU.Avg10 })
	add("psi_memory", "presión de memoria", t.PSIMemory, t.PSIMemory/2, true, func(s collector.Sample) float64 { return s.PSI.Memory.Avg10 })
	add("psi_io", "presión de E/S", t.PSIIO, t.PSIIO/2, true, func(s collector.Sample) float64 { return s.PSI.IO.Avg10 })
	return rules
}

type peakState struct {
	above  int // muestras seguidas por encima de High
	active bool
	start  time.Time
	max    float64
}

// Detector analiza cada muestra del recolector y registra picos y OOM kills.
// Es seguro reconfigurarlo (SetThresholds, SetIgnore) mientras recibe muestras.
type Detector struct {
	store *Store
	topN  int

	mu     sync.Mutex
	rules  []Rule
	state  []peakState
	ignore func(service string) bool
	oom    map[string]uint64 // último contador oom_kill visto por servicio
	seeded bool
}

// NewDetector crea un detector que registra en store.
func NewDetector(store *Store, t Thresholds) *Detector {
	rules := t.Rules()
	return &Detector{
		store: store,
		rules: rules,
		state: make([]peakState, len(rules)),
		oom:   map[string]uint64{},
		topN:  3,
	}
}

// SetThresholds cambia los umbrales. Los picos en curso de un recurso que
// sigue vigilado continúan abiertos; los de un recurso cuya alerta se
// desactiva se cierran con un evento.
func (d *Detector) SetThresholds(t Thresholds) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rules := t.Rules()
	state := make([]peakState, len(rules))
	kept := map[string]bool{}
	for i, r := range rules {
		for j, old := range d.rules {
			if old.Resource == r.Resource {
				state[i] = d.state[j]
				kept[r.Resource] = true
			}
		}
	}
	now := time.Now()
	for j, old := range d.rules {
		if st := d.state[j]; st.active && !kept[old.Resource] {
			dur := now.Sub(st.start).Round(time.Second)
			d.store.Add(Event{
				Time: now, Kind: PeakEnd, Severity: Info,
				Resource: old.Resource, Value: st.max, Threshold: old.High, Duration: dur.String(),
				Message: fmt.Sprintf("pico de %s cerrado tras %s: la alerta se desactivó en la configuración (máximo %.1f%%)", old.Label, dur, st.max),
			})
		}
	}
	d.rules, d.state = rules, state
}

// SetIgnore indica qué servicios no deben generar incidencias. nil: ninguno.
func (d *Detector) SetIgnore(ignore func(service string) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ignore = ignore
}

// Observe procesa una muestra. Tiene la firma de collector.Observer.
func (d *Detector) Observe(s collector.Sample, services []collector.ServiceSample) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var ctx *Context // se construye solo si hace falta
	context := func() *Context {
		if ctx == nil {
			ctx = NewContext(s, services, d.topN)
		}
		return ctx
	}

	for i, r := range d.rules {
		if r.NeedsPSI && !s.PSI.Available {
			continue
		}
		v := r.Value(s)
		st := &d.state[i]

		if !st.active {
			if v < r.High {
				st.above = 0
				continue
			}
			st.above++
			if st.above < r.Sustain {
				continue
			}
			st.active, st.start, st.max = true, s.Time, v
			d.store.Add(Event{
				Time: s.Time, Kind: PeakStart, Severity: Warning,
				Resource: r.Resource, Value: v, Threshold: r.High,
				Message: fmt.Sprintf("pico de %s: %.1f%% (umbral %.0f%%)", r.Label, v, r.High),
				Context: context(),
			})
			continue
		}

		if v > st.max {
			st.max = v
		}
		if v < r.Low {
			dur := s.Time.Sub(st.start).Round(time.Second)
			d.store.Add(Event{
				Time: s.Time, Kind: PeakEnd, Severity: Info,
				Resource: r.Resource, Value: st.max, Threshold: r.High, Duration: dur.String(),
				Message: fmt.Sprintf("fin del pico de %s tras %s (máximo %.1f%%)", r.Label, dur, st.max),
				Context: context(),
			})
			*st = peakState{}
		}
	}

	d.checkOOM(s, services, context)
}

// checkOOM compara el contador oom_kill de cada servicio con el anterior.
func (d *Detector) checkOOM(s collector.Sample, services []collector.ServiceSample, context func() *Context) {
	seen := make(map[string]uint64, len(services))
	for _, svc := range services {
		seen[svc.Name] = svc.OOMKills // se registra aunque se ignore, para no avisar de OOM antiguos si deja de ignorarse
		if d.ignore != nil && d.ignore(svc.Name) {
			continue
		}
		prev, known := d.oom[svc.Name]
		// En la primera muestra solo se toma referencia: los OOM antiguos no son nuevos.
		// Un servicio que aparece después sí se compara contra 0, porque su cgroup es nuevo.
		if !d.seeded || (known && svc.OOMKills <= prev) || (!known && svc.OOMKills == 0) {
			continue
		}
		n := svc.OOMKills - prev
		msg := fmt.Sprintf("el OOM killer mató %d proceso(s) de %s (uso actual %s", n, svc.Label(), formatBytes(svc.MemBytes))
		if svc.MemMax > 0 {
			msg += " de un límite de " + formatBytes(svc.MemMax)
		}
		msg += ")"
		d.store.Add(Event{
			Time: s.Time, Kind: OOMKill, Severity: Critical,
			Service: svc.Name, Value: float64(n), Message: msg, Context: context(),
		})
	}
	d.oom = seen
	d.seeded = true
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
