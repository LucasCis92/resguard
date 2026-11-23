package events

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/metrics"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore("", 100, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func kinds(evs []Event) []Kind {
	out := make([]Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// feed passes the detector a series of CPU values, one per second.
func feed(d *Detector, start time.Time, cpu ...float64) {
	for i, v := range cpu {
		d.Observe(collector.Sample{Time: start.Add(time.Duration(i) * time.Second), CPUPercent: v}, nil)
	}
}

func TestPeakHysteresis(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{CPU: 90, Sustain: 1})
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Goes up to 95, hovers around the threshold (never below 80), then drops.
	feed(d, t0, 50, 95, 88, 97, 85, 91, 70, 50)

	evs := store.Recent(0)
	if len(evs) != 2 || evs[0].Kind != PeakStart || evs[1].Kind != PeakEnd {
		t.Fatalf("events = %v, want a single start and a single end", kinds(evs))
	}
	if evs[0].Value != 95 || evs[0].Resource != "cpu" || evs[0].Context == nil {
		t.Errorf("start = %+v", evs[0])
	}
	end := evs[1]
	if end.Value != 97 {
		t.Errorf("peak maximum = %v, want 97", end.Value)
	}
	if end.Duration != "5s" {
		t.Errorf("duration = %q, want 5s", end.Duration)
	}
}

func TestPeakSustain(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{CPU: 90, Sustain: 3})
	t0 := time.Now()

	feed(d, t0, 95, 95, 50, 95, 95) // never 3 in a row
	if n := len(store.Recent(0)); n != 0 {
		t.Fatalf("brief peaks should not be recorded; events = %d", n)
	}
	feed(d, t0.Add(time.Minute), 95)
	if evs := store.Recent(0); len(evs) != 1 || evs[0].Kind != PeakStart {
		t.Fatalf("events = %v, want a start after 3 samples", kinds(evs))
	}
}

func TestPSIRulesNeedPSI(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{PSIMemory: 10, Sustain: 1})

	d.Observe(collector.Sample{Time: time.Now()}, nil) // PSI not available
	s := collector.Sample{Time: time.Now(), PSI: metrics.PSI{Available: true}}
	s.PSI.Memory.Avg10 = 25
	d.Observe(s, nil)

	evs := store.Recent(0)
	if len(evs) != 1 || evs[0].Resource != "psi_memory" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestOOMKills(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{})
	now := time.Now()
	obs := func(svcs ...collector.ServiceSample) { d.Observe(collector.Sample{Time: now}, svcs) }

	// OOM kills before the daemon started are not new incidents.
	obs(collector.ServiceSample{Name: "db.service", OOMKills: 5})
	if n := len(store.Recent(0)); n != 0 {
		t.Fatalf("the first sample produced %d events", n)
	}

	obs(collector.ServiceSample{Name: "db.service", OOMKills: 7, MemMax: 512 << 20},
		collector.ServiceSample{Name: "web.service", OOMKills: 1}) // a new service with one OOM kill
	obs(collector.ServiceSample{Name: "db.service", OOMKills: 7},
		collector.ServiceSample{Name: "web.service", OOMKills: 1}) // no change

	evs := store.Recent(0)
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Service != "db.service" || evs[0].Value != 2 || evs[0].Kind != OOMKill || evs[0].Severity != Critical {
		t.Errorf("db event = %+v", evs[0])
	}
	if evs[1].Service != "web.service" || evs[1].Value != 1 {
		t.Errorf("web event = %+v", evs[1])
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl")

	s1, err := NewStore(path, 10, quiet)
	if err != nil {
		t.Fatal(err)
	}
	s1.Add(Event{Kind: ServiceFailed, Severity: Critical, Service: "a.service", Message: "one"})
	s1.Add(Event{Kind: PeakStart, Severity: Warning, Message: "two",
		Context: &Context{CPUPercent: 99, TopCPU: []ServiceUsage{{Name: "b.service", CPUPercent: 80}}}})
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(path, 10, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	evs := s2.Recent(0)
	if len(evs) != 2 || evs[0].Message != "one" || evs[1].Context.TopCPU[0].Name != "b.service" {
		t.Fatalf("loaded events = %+v", evs)
	}
	// IDs continue where they left off.
	if e := s2.Add(Event{Message: "three"}); e.ID != 3 {
		t.Errorf("new event ID = %d, want 3", e.ID)
	}
}

func TestNewContextTop(t *testing.T) {
	svcs := []collector.ServiceSample{
		{Name: "a", CPUPercent: 1, MemBytes: 300},
		{Name: "b", CPUPercent: 50, MemBytes: 100},
		{Name: "c", CPUPercent: 20, MemBytes: 200},
	}
	c := NewContext(collector.Sample{}, svcs, 2)
	if len(c.TopCPU) != 2 || c.TopCPU[0].Name != "b" || c.TopCPU[1].Name != "c" {
		t.Errorf("TopCPU = %+v", c.TopCPU)
	}
	if len(c.TopMemory) != 2 || c.TopMemory[0].Name != "a" || c.TopMemory[1].Name != "c" {
		t.Errorf("TopMemory = %+v", c.TopMemory)
	}
	if svcs[0].Name != "a" {
		t.Error("NewContext must not reorder the original list")
	}
}

func TestSetThresholdsKeepsActivePeak(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{CPU: 90, Memory: 90, Sustain: 1})
	t0 := time.Now()

	d.Observe(collector.Sample{Time: t0, CPUPercent: 95, MemUsedPercent: 95}, nil)
	if n := len(store.Recent(0)); n != 2 {
		t.Fatalf("initial events = %d, want 2 starts", n)
	}

	// The CPU threshold changes and the memory one is disabled.
	d.SetThresholds(Thresholds{CPU: 80, Sustain: 1})
	evs := store.Recent(0)
	if len(evs) != 3 || evs[2].Kind != PeakEnd || evs[2].Resource != "memory" || evs[2].Reason != "disabled" || !strings.Contains(evs[2].Message, "disabled") {
		t.Fatalf("disabling memory must close its peak: %+v", kinds(evs))
	}

	// The CPU peak stays open: 85 neither closes it (new Low = 70) nor opens another.
	d.Observe(collector.Sample{Time: t0.Add(time.Second), CPUPercent: 85}, nil)
	if n := len(store.Recent(0)); n != 3 {
		t.Fatalf("there should be no new events: %v", kinds(store.Recent(0)))
	}
	d.Observe(collector.Sample{Time: t0.Add(2 * time.Second), CPUPercent: 50}, nil)
	evs = store.Recent(0)
	if last := evs[len(evs)-1]; last.Kind != PeakEnd || last.Resource != "cpu" || last.Duration != "2s" || last.Value != 95 {
		t.Errorf("end of the CPU peak = %+v", last)
	}
}

func TestIgnoreOOM(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{})
	d.SetIgnore(func(s string) bool { return s == "ruidoso.service" })
	now := time.Now()
	obs := func(n uint64) {
		d.Observe(collector.Sample{Time: now}, []collector.ServiceSample{
			{Name: "ruidoso.service", OOMKills: n}, {Name: "db.service", OOMKills: n},
		})
	}
	obs(0)
	obs(1)
	evs := store.Recent(0)
	if len(evs) != 1 || evs[0].Service != "db.service" {
		t.Fatalf("events = %+v", evs)
	}
	// Once no longer ignored, OOM kills from while it was ignored are not reported.
	d.SetIgnore(nil)
	obs(1)
	if n := len(store.Recent(0)); n != 1 {
		t.Errorf("old OOM kills should not show up: %d events", n)
	}
}
