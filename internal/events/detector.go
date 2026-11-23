package events

import (
	"sync"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
)

// Thresholds are the values above which there is a peak. A value of 0
// disables the corresponding rule.
type Thresholds struct {
	CPU       float64 `yaml:"cpu"`        // CPU usage, %
	Memory    float64 `yaml:"memory"`     // memory in use, %
	PSICPU    float64 `yaml:"psi_cpu"`    // PSI cpu some avg10 (%)
	PSIMemory float64 `yaml:"psi_memory"` // PSI memory some avg10 (%)
	PSIIO     float64 `yaml:"psi_io"`     // PSI io some avg10 (%)
	Sustain   int     `yaml:"sustain"`    // consecutive samples above the threshold to open a peak
}

// DefaultThresholds are sensible values for a general-purpose server.
var DefaultThresholds = Thresholds{
	CPU:       90,
	Memory:    90,
	PSICPU:    40,
	PSIMemory: 10,
	PSIIO:     30,
	Sustain:   2,
}

// Rule describes when a resource peak starts and ends. It uses hysteresis:
// the peak starts when the value reaches High and ends when it drops below
// Low, so a value hovering around the threshold doesn't flood the log.
type Rule struct {
	Resource string
	High     float64
	Low      float64
	Sustain  int
	NeedsPSI bool
	Value    func(collector.Sample) float64
}

// Rules builds the rules from the thresholds.
func (t Thresholds) Rules() []Rule {
	sustain := t.Sustain
	if sustain < 1 {
		sustain = 1
	}
	var rules []Rule
	add := func(resource string, high, margin float64, psi bool, value func(collector.Sample) float64) {
		if high <= 0 {
			return
		}
		rules = append(rules, Rule{
			Resource: resource, High: high, Low: high - margin,
			Sustain: sustain, NeedsPSI: psi, Value: value,
		})
	}
	add("cpu", t.CPU, 10, false, func(s collector.Sample) float64 { return s.CPUPercent })
	add("memory", t.Memory, 5, false, func(s collector.Sample) float64 { return s.MemUsedPercent })
	add("psi_cpu", t.PSICPU, t.PSICPU/2, true, func(s collector.Sample) float64 { return s.PSI.CPU.Avg10 })
	add("psi_memory", t.PSIMemory, t.PSIMemory/2, true, func(s collector.Sample) float64 { return s.PSI.Memory.Avg10 })
	add("psi_io", t.PSIIO, t.PSIIO/2, true, func(s collector.Sample) float64 { return s.PSI.IO.Avg10 })
	return rules
}

type peakState struct {
	above  int // consecutive samples above High
	active bool
	start  time.Time
	max    float64
}

// Detector analyzes every collector sample and records peaks and OOM kills.
// It is safe to reconfigure (SetThresholds, SetIgnore) while it receives
// samples.
type Detector struct {
	store *Store
	topN  int

	mu     sync.Mutex
	rules  []Rule
	state  []peakState
	ignore func(service string) bool
	oom    map[string]uint64 // last oom_kill counter seen per unit
	seeded bool
}

// NewDetector creates a detector that records into store.
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

// SetThresholds changes the thresholds. Ongoing peaks of a resource that is
// still monitored stay open; those of a resource whose alert gets disabled are
// closed with an event.
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
			d.store.Add(Event{
				Time: now, Kind: PeakEnd, Severity: Info, Reason: "disabled",
				Resource: old.Resource, Value: st.max, Threshold: old.High,
				Duration: now.Sub(st.start).Round(time.Second).String(),
			})
		}
	}
	d.rules, d.state = rules, state
}

// SetIgnore sets which units must not raise incidents. nil means none.
func (d *Detector) SetIgnore(ignore func(service string) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ignore = ignore
}

// Observe processes one sample. It has the signature of collector.Observer.
func (d *Detector) Observe(s collector.Sample, services []collector.ServiceSample) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var ctx *Context // built only when needed
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
				Resource: r.Resource, Value: v, Threshold: r.High, Context: context(),
			})
			continue
		}

		if v > st.max {
			st.max = v
		}
		if v < r.Low {
			d.store.Add(Event{
				Time: s.Time, Kind: PeakEnd, Severity: Info,
				Resource: r.Resource, Value: st.max, Threshold: r.High,
				Duration: s.Time.Sub(st.start).Round(time.Second).String(), Context: context(),
			})
			*st = peakState{}
		}
	}

	d.checkOOM(s, services, context)
}

// checkOOM compares each unit's oom_kill counter with the previous one.
func (d *Detector) checkOOM(s collector.Sample, services []collector.ServiceSample, context func() *Context) {
	seen := make(map[string]uint64, len(services))
	for _, svc := range services {
		// Recorded even when ignored, so that old OOM kills are not reported
		// if the unit stops being ignored.
		seen[svc.Name] = svc.OOMKills
		if d.ignore != nil && d.ignore(svc.Name) {
			continue
		}
		prev, known := d.oom[svc.Name]
		// The first sample is only a baseline: earlier OOM kills are not new.
		// A unit that shows up later is compared against 0, since its cgroup
		// is new.
		if !d.seeded || (known && svc.OOMKills <= prev) || (!known && svc.OOMKills == 0) {
			continue
		}
		d.store.Add(Event{
			Time: s.Time, Kind: OOMKill, Severity: Critical,
			Service: svc.Name, ServiceLabel: svc.Label(), Value: float64(svc.OOMKills - prev),
			MemBytes: svc.MemBytes, MemMax: svc.MemMax, Context: context(),
		})
	}
	d.oom = seen
	d.seeded = true
}
