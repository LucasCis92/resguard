package metrics

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestReadServicesUserSessionsAndContainers(t *testing.T) {
	root := t.TempDir()
	usr := "sys/fs/cgroup/user.slice/user-1000.slice/"
	mgr := usr + "user@1000.service/"
	id := "74a85415a2bd981099746ca31e2caea85fb944f46034307c0510e88129a533b9"
	other := "95ebfa4a993d6f35dda48b16cb925e7616756590e9d6030a31d9cc1adc64890b"
	files := map[string]string{
		"var/lib/docker/containers/" + id + "/config.v2.json": `{"ID":"x","Name":"/laravel_app","Config":{}}`,
	}
	for _, unit := range []string{
		usr + "session-6.scope",
		mgr + "init.scope",
		mgr + "app.slice/app-com.google.Chrome-5016.scope",
		mgr + `app.slice/app-gnome-google\x2dchrome-5016.scope`,
		mgr + "app.slice/app-gnome-code-6255.scope",
		mgr + "app.slice/app-org.gnome.Terminal.slice/vte-spawn-3f2a9c1e.scope",
		mgr + "session.slice/org.gnome.Shell@wayland.service",
		"sys/fs/cgroup/system.slice/docker-" + id + ".scope",
		"sys/fs/cgroup/system.slice/docker-" + other + ".scope",
		"sys/fs/cgroup/system.slice/nginx.service",
	} {
		files[unit+"/cpu.stat"] = "usage_usec 100\n"
	}
	writeFiles(t, root, files)

	svcs, err := (&Reader{Root: root}).ReadServices([]string{"system.slice", "user.slice"})
	if err != nil {
		t.Fatal(err)
	}
	type kd struct{ kind, display string }
	got := map[string]kd{}
	for _, s := range svcs {
		got[s.Name] = kd{s.Kind, s.Display}
	}
	want := map[string]kd{
		"session-6.scope":                       {KindSession, "sesión 6"},
		"init.scope":                            {KindOther, "init.scope"},
		"app-com.google.Chrome-5016.scope":      {KindApp, "Chrome"},
		`app-gnome-google\x2dchrome-5016.scope`: {KindApp, "Chrome"}, // misma apertura (5016) que la anterior
		"app-gnome-code-6255.scope":             {KindApp, "code"},
		"vte-spawn-3f2a9c1e.scope":              {KindApp, "Terminal"},
		"org.gnome.Shell@wayland.service":       {KindService, "org.gnome.Shell@wayland.service"},
		"docker-" + id + ".scope":               {KindContainer, "laravel_app"},
		"docker-" + other + ".scope":            {KindContainer, "contenedor 95ebfa4a993d"},
		"nginx.service":                         {KindService, "nginx.service"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unidades:\n got  %v\n want %v", got, want)
	}
	if _, ok := got["user@1000.service"]; ok {
		t.Error("user@1000.service debe recorrerse, no contarse como una unidad")
	}
}

func TestAppName(t *testing.T) {
	for unit, want := range map[string]string{
		"app-com.google.Chrome-5016.scope":                                "Chrome",
		"app-gnome-code-6255.scope":                                       "code",
		"app-code-6255.scope":                                             "code",
		`app-gnome-update\x2dnotifier-5621.scope`:                         "update-notifier",
		"app-gnome-org.gnome.SettingsDaemon.DiskUtilityNotify-4266.scope": "DiskUtilityNotify",
		"app-flatpak-com.spotify.Client-1234.scope":                       "Client",
		"app-gnome-firefox-a1b2c3.scope":                                  "firefox",
		`app-gnome-xdg\x2duser\x2ddirs@autostart.service`:                 "xdg-user-dirs",
		"app-org.gnome.Terminal.slice":                                    "Terminal",
		"app-.scope":                                                      "app-.scope",
	} {
		if got := appName(unit); got != want {
			t.Errorf("appName(%q) = %q, quiero %q", unit, got, want)
		}
	}
}

func TestReadProcesses(t *testing.T) {
	root := t.TempDir()
	app := "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.google.Chrome-5016.scope"
	writeFiles(t, root, map[string]string{
		app + "/cgroup.procs":       "100\n200\n",
		app + "/child/cgroup.procs": "300\n400\n",
		// Proceso con espacios y paréntesis en el nombre: se parsea desde el último ")".
		"proc/100/stat":    "100 (chrome (x) y) S 1 100 100 0 -1 4194304 1 0 0 0 250 50 0 0 20 0 31 0 1 1 1\n",
		"proc/100/comm":    "chrome\n",
		"proc/100/statm":   "1000 256 10 1 0 100 0\n",
		"proc/100/status":  "Name:\tchrome\nUid:\t0\t0\t0\t0\n",
		"proc/100/cmdline": "/opt/google/chrome/chrome\x00--password=secreto\x00https://meet.example/abc?token=xyz\x00",
		"proc/200/stat":    "200 (chrome) S 100 100 100 0 -1 0 0 0 0 0 1000 200 0 0 20 0 12 0 1 1 1\n",
		"proc/200/comm":    "chrome\n",
		"proc/200/statm":   "2000 512 10 1 0 100 0\n",
		"proc/200/status":  "Uid:\t0\t0\t0\t0\n",
		"proc/200/cmdline": "/opt/google/chrome/chrome\x00--type=utility\x00--utility-sub-type=audio.mojom.AudioService\x00--lang=es\x00",
		// Formato real de Chromium: todo en un bloque separado por espacios.
		"proc/300/stat":    "300 (chrome) S 100 100 100 0 -1 0 0 0 0 0 10 5 0 0 20 0 8 0 1 1 1\n",
		"proc/300/comm":    "chrome\n",
		"proc/300/statm":   "100 50 10 1 0 100 0\n",
		"proc/300/status":  "Uid:\t0\t0\t0\t0\n",
		"proc/300/cmdline": "/opt/google/chrome/chrome --type=renderer --crashpad-handler-pid=12 --lang=es\x00",
		// El 400 terminó entre la lectura de cgroup.procs y la de /proc: se omite.
	})

	procs, err := (&Reader{Root: root}).ReadProcesses(strings.TrimPrefix(app, "sys/fs/cgroup/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 3 {
		t.Fatalf("procesos = %+v", procs)
	}
	if r := procs[2]; r.PID != 300 || r.Role != "renderer" {
		t.Errorf("proceso 300 (línea de órdenes separada por espacios) = %+v", r)
	}
	page := uint64(os.Getpagesize())
	p := procs[0]
	if p.PID != 100 || p.CPUTicks != 300 || p.Threads != 31 || p.RSSBytes != 256*page || p.Name != "chrome" || p.User != "root" || p.Role != "" {
		t.Errorf("proceso 100 = %+v", p)
	}
	if q := procs[1]; q.CPUTicks != 1200 || q.Role != "utility: audio" {
		t.Errorf("proceso 200 = %+v", q)
	}
	for _, pr := range procs {
		dump := fmt.Sprintf("%+v", pr)
		for _, secret := range []string{"secreto", "token", "meet.example", "password", "/opt/google"} {
			if strings.Contains(dump, secret) {
				t.Errorf("la información del proceso no debe incluir argumentos (%q): %s", secret, dump)
			}
		}
	}
}

func TestReadProcessesRejectsEscapes(t *testing.T) {
	if _, err := (&Reader{Root: t.TempDir()}).ReadProcesses("../../etc"); err == nil {
		t.Error("una ruta que sale de la jerarquía de cgroups debe rechazarse")
	}
}

func TestProcessRole(t *testing.T) {
	for args, want := range map[string]string{
		"chrome --type=renderer --lang=es": "renderer",
		"chrome --type=gpu-process":        "gpu-process",
		"chrome --type=utility --utility-sub-type=video_capture.mojom.VideoCaptureService": "utility: video_capture",
		"chrome --type=utility --utility-sub-type=network.mojom.NetworkService":            "utility: network",
		"chrome --type=utility":          "utility",
		"code --type=zygote":             "zygote",
		"chrome https://meet.example":    "",
		"x --type=renderer;rm -rf $HOME": "rendererrm", // solo letras, números, - _ .
	} {
		if got := processRole(strings.Fields(args)); got != want {
			t.Errorf("processRole(%q) = %q, quiero %q", args, got, want)
		}
	}
}
