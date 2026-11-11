package events

import (
	"io"
	"log/slog"
	"path/filepath"
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

// feed pasa al detector una serie de valores de CPU, uno por segundo.
func feed(d *Detector, start time.Time, cpu ...float64) {
	for i, v := range cpu {
		d.Observe(collector.Sample{Time: start.Add(time.Duration(i) * time.Second), CPUPercent: v}, nil)
	}
}

func TestPeakHysteresis(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{CPU: 90, Sustain: 1})
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Sube a 95, oscila alrededor del umbral (sin bajar de 80) y luego baja.
	feed(d, t0, 50, 95, 88, 97, 85, 91, 70, 50)

	evs := store.Recent(0)
	if len(evs) != 2 || evs[0].Kind != PeakStart || evs[1].Kind != PeakEnd {
		t.Fatalf("eventos = %v, quiero un único inicio y un único fin", kinds(evs))
	}
	if evs[0].Value != 95 || evs[0].Resource != "cpu" || evs[0].Context == nil {
		t.Errorf("inicio = %+v", evs[0])
	}
	end := evs[1]
	if end.Value != 97 {
		t.Errorf("máximo del pico = %v, quiero 97", end.Value)
	}
	if end.Duration != "5s" {
		t.Errorf("duración = %q, quiero 5s", end.Duration)
	}
}

func TestPeakSustain(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{CPU: 90, Sustain: 3})
	t0 := time.Now()

	feed(d, t0, 95, 95, 50, 95, 95) // nunca 3 seguidas
	if n := len(store.Recent(0)); n != 0 {
		t.Fatalf("picos breves no deberían registrarse; eventos = %d", n)
	}
	feed(d, t0.Add(time.Minute), 95)
	if evs := store.Recent(0); len(evs) != 1 || evs[0].Kind != PeakStart {
		t.Fatalf("eventos = %v, quiero un inicio tras 3 muestras", kinds(evs))
	}
}

func TestPSIRulesNeedPSI(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{PSIMemory: 10, Sustain: 1})

	d.Observe(collector.Sample{Time: time.Now()}, nil) // PSI no disponible
	s := collector.Sample{Time: time.Now(), PSI: metrics.PSI{Available: true}}
	s.PSI.Memory.Avg10 = 25
	d.Observe(s, nil)

	evs := store.Recent(0)
	if len(evs) != 1 || evs[0].Resource != "psi_memory" {
		t.Fatalf("eventos = %+v", evs)
	}
}

func TestOOMKills(t *testing.T) {
	store := newStore(t)
	d := NewDetector(store, Thresholds{})
	now := time.Now()
	obs := func(svcs ...collector.ServiceSample) { d.Observe(collector.Sample{Time: now}, svcs) }

	// Los OOM previos al arranque del demonio no son incidencias nuevas.
	obs(collector.ServiceSample{Name: "db.service", OOMKills: 5})
	if n := len(store.Recent(0)); n != 0 {
		t.Fatalf("primer muestreo generó %d eventos", n)
	}

	obs(collector.ServiceSample{Name: "db.service", OOMKills: 7, MemMax: 512 << 20},
		collector.ServiceSample{Name: "web.service", OOMKills: 1}) // servicio nuevo con un OOM
	obs(collector.ServiceSample{Name: "db.service", OOMKills: 7},
		collector.ServiceSample{Name: "web.service", OOMKills: 1}) // sin cambios

	evs := store.Recent(0)
	if len(evs) != 2 {
		t.Fatalf("eventos = %+v", evs)
	}
	if evs[0].Service != "db.service" || evs[0].Value != 2 || evs[0].Kind != OOMKill || evs[0].Severity != Critical {
		t.Errorf("evento db = %+v", evs[0])
	}
	if evs[1].Service != "web.service" || evs[1].Value != 1 {
		t.Errorf("evento web = %+v", evs[1])
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl")

	s1, err := NewStore(path, 10, quiet)
	if err != nil {
		t.Fatal(err)
	}
	s1.Add(Event{Kind: ServiceFailed, Severity: Critical, Service: "a.service", Message: "uno"})
	s1.Add(Event{Kind: PeakStart, Severity: Warning, Message: "dos",
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
	if len(evs) != 2 || evs[0].Message != "uno" || evs[1].Context.TopCPU[0].Name != "b.service" {
		t.Fatalf("eventos recuperados = %+v", evs)
	}
	// Los IDs continúan donde se quedaron.
	if e := s2.Add(Event{Message: "tres"}); e.ID != 3 {
		t.Errorf("ID del nuevo evento = %d, quiero 3", e.ID)
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
		t.Error("NewContext no debe reordenar la lista original")
	}
}
