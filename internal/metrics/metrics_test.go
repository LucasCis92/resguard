package metrics

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeFiles crea un árbol de ficheros falso bajo root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fakeProc(t *testing.T) string {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"proc/stat":    "cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 100 0 50 800 50 0 0 0 0 0\n",
		"proc/loadavg": "1.50 0.75 0.25 2/300 12345\n",
		"proc/meminfo": "MemTotal:       16000000 kB\nMemFree:         1000000 kB\n" +
			"MemAvailable:    4000000 kB\nSwapTotal:       2000000 kB\nSwapFree:        1500000 kB\n",
		"proc/pressure/cpu":    "some avg10=12.50 avg60=5.00 avg300=1.00 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		"proc/pressure/memory": "some avg10=3.00 avg60=2.00 avg300=1.00 total=1\nfull avg10=1.50 avg60=1.00 avg300=0.50 total=1\n",
		"proc/pressure/io":     "some avg10=7.25 avg60=0.00 avg300=0.00 total=1\nfull avg10=4.00 avg60=0.00 avg300=0.00 total=1\n",
	})
	return root
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestReadSystem(t *testing.T) {
	s, err := (&Reader{Root: fakeProc(t)}).ReadSystem()
	if err != nil {
		t.Fatal(err)
	}
	if s.Load1 != 1.5 || s.Load5 != 0.75 || s.Load15 != 0.25 {
		t.Errorf("carga = %v %v %v", s.Load1, s.Load5, s.Load15)
	}
	if s.MemTotalKB != 16000000 || s.MemAvailableKB != 4000000 {
		t.Errorf("memoria = %d/%d", s.MemAvailableKB, s.MemTotalKB)
	}
	if got := s.MemUsedPercent(); !near(got, 75) {
		t.Errorf("MemUsedPercent = %v, quiero 75", got)
	}
	if got := s.SwapUsedPercent(); !near(got, 25) {
		t.Errorf("SwapUsedPercent = %v, quiero 25", got)
	}
	if s.CPU.Total() != 1000 || s.CPU.IdleTotal() != 850 {
		t.Errorf("CPU total/idle = %d/%d", s.CPU.Total(), s.CPU.IdleTotal())
	}
	if !s.PSI.Available || s.PSI.CPU.Avg10 != 12.5 || s.PSI.IO.Avg10 != 7.25 ||
		s.PSI.MemoryFull.Avg10 != 1.5 || s.PSI.IOFull.Avg10 != 4 {
		t.Errorf("PSI = %+v", s.PSI)
	}
}

func TestReadSystemWithoutPSI(t *testing.T) {
	root := fakeProc(t)
	if err := os.RemoveAll(filepath.Join(root, "proc/pressure")); err != nil {
		t.Fatal(err)
	}
	s, err := (&Reader{Root: root}).ReadSystem()
	if err != nil {
		t.Fatalf("sin PSI no debería fallar: %v", err)
	}
	if s.PSI.Available {
		t.Error("PSI.Available debería ser false")
	}
}

func TestCPUPercent(t *testing.T) {
	prev := CPUTimes{User: 100, System: 50, Idle: 800, IOWait: 50}
	cur := CPUTimes{User: 250, System: 100, Idle: 900, IOWait: 50} // +200 ocupado, +100 ocioso
	if got := CPUPercent(prev, cur); !near(got, 200.0/300*100) {
		t.Errorf("CPUPercent = %v", got)
	}
	if got := CPUPercent(cur, cur); got != 0 {
		t.Errorf("sin cambios: CPUPercent = %v, quiero 0", got)
	}
	// Un contador que retrocede (p. ej. CPU desconectada) no debe dar valores absurdos.
	if got := CPUPercent(cur, prev); got < 0 || got > 100 {
		t.Errorf("contadores decrecientes: CPUPercent = %v", got)
	}
}

func TestReadServices(t *testing.T) {
	root := t.TempDir()
	cg := "sys/fs/cgroup/system.slice/"
	writeFiles(t, root, map[string]string{
		cg + "nginx.service/cpu.stat":       "usage_usec 5000\nuser_usec 3000\n",
		cg + "nginx.service/memory.current": "1048576\n",
		cg + "nginx.service/memory.max":     "max\n",
		cg + "nginx.service/memory.events":  "low 0\nhigh 0\nmax 4\noom 2\noom_kill 1\n",
		// Servicio en un slice anidado y con límite de memoria.
		cg + "system-getty.slice/getty@tty1.service/cpu.stat":       "usage_usec 10\n",
		cg + "system-getty.slice/getty@tty1.service/memory.current": "4096\n",
		cg + "system-getty.slice/getty@tty1.service/memory.max":     "8192\n",
		// Subcgroup dentro de un servicio: no es un servicio aparte.
		cg + "nginx.service/worker.service/cpu.stat": "usage_usec 1\n",
		// Otros tipos de unidad se ignoran.
		cg + "boot.mount/cpu.stat": "usage_usec 1\n",
	})

	svcs, err := (&Reader{Root: root}).ReadServices([]string{"system.slice", "no-existe.slice"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Service{}
	for _, s := range svcs {
		got[s.Name] = s
	}
	if len(got) != 2 {
		t.Fatalf("servicios = %v, quiero nginx y getty", svcs)
	}
	n := got["nginx.service"]
	if n.CPUUsageUsec != 5000 || n.MemCurrent != 1048576 || n.MemMax != 0 || n.OOM != 2 || n.OOMKills != 1 {
		t.Errorf("nginx = %+v", n)
	}
	g := got["getty@tty1.service"]
	if g.MemMax != 8192 || g.Cgroup != "system.slice/system-getty.slice/getty@tty1.service" {
		t.Errorf("getty = %+v", g)
	}
}
