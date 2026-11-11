package collector

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/LucasCis92/resguard/internal/metrics"
)

func TestRing(t *testing.T) {
	r := NewRing[int](3)
	if got := r.Last(0); len(got) != 0 {
		t.Errorf("vacío: %v", got)
	}
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	if r.Len() != 3 {
		t.Errorf("Len = %d", r.Len())
	}
	if got := r.Last(0); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Errorf("Last(0) = %v", got)
	}
	if got := r.Last(2); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Errorf("Last(2) = %v", got)
	}
	if got := r.Last(10); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Errorf("Last(10) = %v", got)
	}
}

// fakeRoot es un /proc y /sys/fs/cgroup mínimos cuyos contadores se pueden avanzar.
type fakeRoot struct {
	t    *testing.T
	root string
}

func (f fakeRoot) write(name, content string) {
	p := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f fakeRoot) set(busy, idle, svcUsec, oomKills uint64) {
	f.write("proc/stat", fmt.Sprintf("cpu %d 0 0 %d 0 0 0 0\n", busy, idle))
	f.write("proc/loadavg", "0.5 0.5 0.5 1/1 1\n")
	f.write("proc/meminfo", "MemTotal: 1000 kB\nMemAvailable: 400 kB\n")
	f.write("sys/fs/cgroup/system.slice/app.service/cpu.stat", fmt.Sprintf("usage_usec %d\n", svcUsec))
	f.write("sys/fs/cgroup/system.slice/app.service/memory.current", "2048\n")
	f.write("sys/fs/cgroup/system.slice/app.service/memory.events", fmt.Sprintf("oom_kill %d\n", oomKills))
}

func TestCollectorTick(t *testing.T) {
	f := fakeRoot{t, t.TempDir()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(Config{History: 10, Slices: []string{"system.slice"}}, &metrics.Reader{Root: f.root}, log)

	var observed []Sample
	c.Observe(func(s Sample, _ []ServiceSample) { observed = append(observed, s) })

	f.set(0, 0, 0, 0)
	c.Tick()
	if _, ok := c.Latest(); ok || len(observed) != 0 {
		t.Fatal("la primera lectura solo debe servir de referencia")
	}

	f.set(300, 100, 1000, 1) // 75 % ocupado
	c.Tick()
	s, ok := c.Latest()
	if !ok {
		t.Fatal("no hay muestra tras el segundo tick")
	}
	if s.CPUPercent != 75 {
		t.Errorf("CPUPercent = %v, quiero 75", s.CPUPercent)
	}
	if s.MemUsedPercent != 60 {
		t.Errorf("MemUsedPercent = %v, quiero 60", s.MemUsedPercent)
	}
	if len(observed) != 1 {
		t.Errorf("observadores llamados %d veces", len(observed))
	}

	svcs := c.Services()
	if len(svcs) != 1 || svcs[0].Name != "app.service" || svcs[0].MemBytes != 2048 || svcs[0].OOMKills != 1 {
		t.Errorf("servicios = %+v", svcs)
	}
	if svcs[0].CPUPercent <= 0 {
		t.Errorf("el servicio consumió CPU pero CPUPercent = %v", svcs[0].CPUPercent)
	}

	f.set(400, 200, 1000, 1)
	c.Tick()
	if h := c.History(0); len(h) != 2 || h[1].CPUPercent != 50 {
		t.Errorf("historial = %+v", h)
	}
}
