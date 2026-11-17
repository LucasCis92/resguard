package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
)

func testCompleter() *completer {
	return &completer{
		commands: func() []string { return []string{"status", "services", "events", "event", "exit", "help", "history"} },
		args:     map[string][]string{"services": {"cpu", "mem"}},
	}
}

func TestComplete(t *testing.T) {
	tests := []struct {
		name, line string
		pos        int // -1: al final
		wantLine   string
		wantOpts   []string
	}{
		{"única opción", "sta", -1, "status ", nil},
		{"prefijo común", "ev", -1, "event", nil},
		{"varias sin avance", "event", -1, "event", []string{"event", "events"}},
		{"sin coincidencias", "zzz", -1, "zzz", nil},
		{"argumento", "services m", -1, "services mem ", nil},
		{"argumentos posibles", "services ", -1, "services ", []string{"cpu", "mem"}},
		{"help completa órdenes", "help hi", -1, "help history ", nil},
		{"orden sin argumentos conocidos", "status x", -1, "status x", nil},
		{"segundo argumento no se completa", "services cpu 1", -1, "services cpu 1", nil},
		{"cursor en medio", "st status", 2, "status status", nil},
	}
	c := testCompleter()
	for _, tt := range tests {
		pos := tt.pos
		if pos < 0 {
			pos = len(tt.line)
		}
		line, newPos, opts := c.complete(tt.line, pos)
		if line != tt.wantLine || !reflect.DeepEqual(opts, tt.wantOpts) {
			t.Errorf("%s: complete(%q) = %q, %v; quiero %q, %v", tt.name, tt.line, line, opts, tt.wantLine, tt.wantOpts)
		}
		if newPos < 0 || newPos > len(line) {
			t.Errorf("%s: posición fuera de rango: %d", tt.name, newPos)
		}
	}
}

func TestHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "estado", "resguard", "history")
	h := loadHistory(path, 3)

	for _, e := range []string{"status", "status ", "", "   ", " secreto", "events", "services mem ", "history 5"} {
		h.Add(e)
	}
	// Sin vacías, sin duplicado consecutivo, sin la que empieza por espacio, máximo 3.
	if got := []string{h.At(0), h.At(1), h.At(2)}; h.Len() != 3 || !reflect.DeepEqual(got, []string{"history 5", "services mem", "events"}) {
		t.Fatalf("historial = %d %v", h.Len(), got)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("permisos del historial = %o, quiero 600", perm)
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Errorf("permisos del directorio = %o, quiero 700", di.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secreto") {
		t.Error("una línea que empieza por espacio no debe guardarse")
	}

	// Al recargar se recupera y se recorta al máximo.
	h2 := loadHistory(path, 3)
	if h2.Len() != 3 || h2.At(0) != "history 5" || h2.At(2) != "events" {
		t.Errorf("historial recargado = %v", h2.entries)
	}
	data, _ = os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 3 {
		t.Errorf("el fichero debería quedar recortado a 3 líneas, tiene %d", n)
	}
}

func TestHistoryWithoutFile(t *testing.T) {
	h := loadHistory("", 10)
	h.Add("status")
	if h.Len() != 1 || h.At(0) != "status" {
		t.Errorf("historial en memoria = %v", h.entries)
	}
}

func TestTruncateANSI(t *testing.T) {
	colored := "ab" + ansiRed + "cdé" + ansiReset + "fg"
	if got := truncateANSI(colored, 4, true); got != "ab"+ansiRed+"cd"+ansiReset {
		t.Errorf("con color = %q", got)
	}
	if got := truncateANSI("ñandú", 3, false); got != "ñan" {
		t.Errorf("multibyte = %q", got)
	}
	if got := truncateANSI("corto", 10, false); got != "corto" {
		t.Errorf("sin recorte = %q", got)
	}
}

func testSnapshot() ipc.Snapshot {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	s := collector.Sample{
		Time: now, CPUPercent: 95, Load1: 3.5, MemUsedPercent: 40,
		MemTotalKB: 16 << 20, MemAvailableKB: 10 << 20,
		PSI: metrics.PSI{Available: true},
	}
	s.PSI.Memory.Avg10 = 2.5
	return ipc.Snapshot{
		Sample: s,
		Services: []collector.ServiceSample{
			{Name: "small.service", CPUPercent: 1, MemBytes: 900 << 20},
			{Name: "busy.service", CPUPercent: 50, MemBytes: 10 << 20, MemMax: 512 << 20},
			{Name: "un-servicio-con-un-nombre-larguísimo-que-no-entra.service", CPUPercent: 2},
		},
		Events: []events.Event{
			{Time: now.Add(-48 * time.Hour), Severity: events.Warning, Message: "pico viejo"},
			{Time: now.Add(-time.Minute), Severity: events.Critical, Message: "nginx.service se cayó"},
		},
		Interval: 2 * time.Second,
	}
}

func plain(frame string) []string {
	return strings.Split(strings.ReplaceAll(frame, "\x1b[K", ""), "\r\n")
}

func TestRender(t *testing.T) {
	snap := testSnapshot()
	v := view{width: 100, height: 30, sortBy: "cpu", now: snap.Sample.Time, refresh: 2 * time.Second}
	lines := plain(render(snap, nil, v))
	frame := strings.Join(lines, "\n")

	for _, want := range []string{"CPU", "95.0%", "carga 3.50", "6.0 GiB de 16.0 GiB", "memoria 2.5%", "512.0 MiB", "(por CPU)", "ÚLTIMAS INCIDENCIAS"} {
		if !strings.Contains(frame, want) {
			t.Errorf("falta %q en:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "\x1b[3") {
		t.Error("sin color no debe haber secuencias de color")
	}
	if strings.Index(frame, "busy.service") > strings.Index(frame, "small.service") {
		t.Error("ordenado por CPU, busy debe ir antes que small")
	}
	// La incidencia más reciente va primero; la de otro día lleva fecha.
	if strings.Index(frame, "se cayó") > strings.Index(frame, "pico viejo") || !strings.Contains(frame, "10/11 10:00") {
		t.Errorf("orden o formato de incidencias incorrecto:\n%s", frame)
	}
	for i, l := range lines {
		if n := utf8.RuneCountInString(l); n > v.width {
			t.Errorf("línea %d mide %d > %d: %q", i, n, v.width, l)
		}
	}

	v.sortBy = "mem"
	frame = strings.Join(plain(render(snap, nil, v)), "\n")
	if strings.Index(frame, "small.service") > strings.Index(frame, "busy.service") || !strings.Contains(frame, "(por memoria)") {
		t.Error("ordenado por memoria, small debe ir antes que busy")
	}
}

func TestRenderSmallTerminal(t *testing.T) {
	snap := testSnapshot()
	v := view{width: 40, height: 12, sortBy: "cpu", color: true, now: snap.Sample.Time}
	frame := render(snap, nil, v)
	lines := plain(frame)
	if len(lines) > v.height {
		t.Errorf("%d líneas en una terminal de %d", len(lines), v.height)
	}
	if !strings.Contains(frame, ansiRed) {
		t.Error("con 95% de CPU la barra debería ir en rojo")
	}
}

func TestRenderError(t *testing.T) {
	v := view{width: 80, height: 24, now: time.Now()}
	frame := strings.Join(plain(render(ipc.Snapshot{}, errors.New("conexión rechazada"), v)), "\n")
	if !strings.Contains(frame, "conexión rechazada") || !strings.Contains(frame, "Reintentando") {
		t.Errorf("fotograma de error:\n%s", frame)
	}
}

func TestLogsArgs(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{nil, "--unit resguard.service --lines 50 --no-hostname --output short --no-pager"},
		{[]string{"200"}, "--unit resguard.service --lines 200 --no-hostname --output short --no-pager"},
		{[]string{"avisos", "seguir"}, "--unit resguard.service --lines 50 --no-hostname --output short --priority warning --follow"},
		{[]string{"seguir", "10"}, "--unit resguard.service --lines 10 --no-hostname --output short --follow"},
	}
	for _, tt := range tests {
		got, err := logsArgs(tt.args)
		if err != nil || strings.Join(got, " ") != tt.want {
			t.Errorf("logsArgs(%v) = %v, %v; quiero %s", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range [][]string{{"todo"}, {"-5"}, {"0"}, {"; rm -rf /"}} {
		if _, err := logsArgs(bad); err == nil {
			t.Errorf("logsArgs(%v) debería fallar", bad)
		}
	}
}
