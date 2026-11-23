package main

import (
	"errors"
	"fmt"
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
			{Name: "small.service", Kind: metrics.KindService, CPUPercent: 1, MemBytes: 900 << 20},
			{Name: "busy.service", Kind: metrics.KindService, CPUPercent: 50, MemBytes: 10 << 20, MemMax: 512 << 20},
			{Name: "un-servicio-con-un-nombre-larguísimo-que-no-entra.service", Kind: metrics.KindService, CPUPercent: 2},
			{Name: "app-com.google.Chrome-5016.scope", Display: "Chrome", Kind: metrics.KindApp, CPUPercent: 30, MemBytes: 2 << 30},
			{Name: `app-gnome-google\x2dchrome-5016.scope`, Display: "Chrome", Kind: metrics.KindApp, CPUPercent: 5, MemBytes: 1 << 30},
			{Name: "app-gnome-code-6255.scope", Display: "code", Kind: metrics.KindApp, CPUPercent: 20, MemBytes: 3 << 30},
			{Name: "docker-abc.scope", Display: "laravel_app", Kind: metrics.KindContainer, CPUPercent: 0.5, MemBytes: 50 << 20},
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

func testView(snap ipc.Snapshot) view {
	return view{width: 110, height: 30, now: snap.Sample.Time, refresh: 2 * time.Second}
}

// selectedLine devuelve la fila marcada como elegida.
func selectedLine(frame string) string {
	for _, l := range plain(frame) {
		if strings.HasPrefix(l, "› ") {
			return l
		}
	}
	return ""
}

func TestRenderUnits(t *testing.T) {
	snap := testSnapshot()
	v := testView(snap)
	st := newWatchState()
	lines := plain(st.render(snap, nil, v))
	frame := strings.Join(lines, "\n")

	for _, want := range []string{"CPU", "95.0%", "carga 3.50", "6.0 GiB de 16.0 GiB", "memoria 2.5%", "512.0 MiB",
		"(por CPU · todo)", "ÚLTIMAS INCIDENCIAS", "Enter procesos", "laravel_app", "contenedor"} {
		if !strings.Contains(frame, want) {
			t.Errorf("falta %q en:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "\x1b[") {
		t.Error("sin color no debe haber secuencias de color")
	}
	// Chrome aparece una vez, con el consumo de sus dos unidades sumado.
	if n := strings.Count(frame, "Chrome"); n != 1 || !strings.Contains(frame, "35.0") {
		t.Errorf("Chrome debería aparecer una vez con 35%% de CPU:\n%s", frame)
	}
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "busy.service") {
		t.Error("sin elegir nada, la fila marcada debe ser la primera (busy.service)")
	}
	if strings.Index(frame, "se cayó") > strings.Index(frame, "pico viejo") || !strings.Contains(frame, "10/11 10:00") {
		t.Errorf("orden o formato de incidencias incorrecto:\n%s", frame)
	}
	for i, l := range lines {
		if n := utf8.RuneCountInString(l); n > v.width {
			t.Errorf("línea %d mide %d > %d: %q", i, n, v.width, l)
		}
	}

	st.sortBy = "mem"
	frame = strings.Join(plain(st.render(snap, nil, v)), "\n")
	if strings.Index(frame, "code") > strings.Index(frame, "small.service") || !strings.Contains(frame, "(por memoria") {
		t.Error("ordenado por memoria, code (3 GiB) debe ir antes que small (900 MiB)")
	}
}

func TestWatchKeys(t *testing.T) {
	snap := testSnapshot()
	v := testView(snap)
	st := newWatchState()
	press := func(keys ...keypress) action {
		var a action
		for _, k := range keys {
			a = st.handle(k, snap, v)
		}
		return a
	}
	r := func(c rune) keypress { return keypress{code: keyRune, r: c} }

	// Filtro: solo aplicaciones.
	press(r('a'))
	frame := strings.Join(plain(st.render(snap, nil, v)), "\n")
	if strings.Contains(frame, "busy.service") || strings.Contains(frame, "laravel_app") || !strings.Contains(frame, "solo apps") {
		t.Errorf("con el filtro de apps no deben verse servicios ni contenedores:\n%s", frame)
	}
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "Chrome") {
		t.Error("la primera app (Chrome, más CPU) debería estar elegida")
	}

	// Bajar una fila y abrir sus procesos.
	press(keypress{code: keyDown})
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "code") {
		t.Error("tras ↓ debería estar elegida code")
	}
	press(keypress{code: keyUp})
	if a := press(keypress{code: keyEnter}); a != actionRefresh || st.mode != modeProcs {
		t.Fatalf("Enter debe abrir la vista de procesos y pedir datos: %v %v", a, st.mode)
	}
	if len(st.target.Units) != 2 {
		t.Errorf("Chrome tiene dos unidades y deben pedirse los procesos de ambas: %v", st.target.Units)
	}

	// Esc vuelve; la selección y el filtro se conservan.
	press(keypress{code: keyEsc})
	if st.mode != modeUnits || !strings.Contains(selectedLine(st.render(snap, nil, v)), "Chrome") {
		t.Error("Esc debe volver a la lista con la misma fila elegida")
	}

	// Contenedores, y después todo.
	press(r('d'))
	frame = strings.Join(plain(st.render(snap, nil, v)), "\n")
	if !strings.Contains(frame, "laravel_app") || strings.Contains(frame, "Chrome") {
		t.Errorf("filtro de contenedores:\n%s", frame)
	}
	press(r('t'))
	if frame = strings.Join(plain(st.render(snap, nil, v)), "\n"); !strings.Contains(frame, "busy.service") {
		t.Error("t debe volver a mostrar todo")
	}

	if press(r('q')) != actionQuit || press(keypress{code: keyEsc}) != actionQuit {
		t.Error("q y Esc en la lista deben salir")
	}
}

func TestWatchScroll(t *testing.T) {
	snap := testSnapshot()
	snap.Services = nil
	for i := 0; i < 50; i++ {
		snap.Services = append(snap.Services, collector.ServiceSample{
			Name: fmt.Sprintf("s%02d.service", i), Kind: metrics.KindService, CPUPercent: float64(100 - i)})
	}
	v := testView(snap)
	v.height = 20
	st := newWatchState()
	st.handle(keypress{code: keyEnd}, snap, v)
	frame := st.render(snap, nil, v)
	if !strings.Contains(selectedLine(frame), "s49.service") {
		t.Errorf("tras Fin la última fila debe verse elegida:\n%s", strings.Join(plain(frame), "\n"))
	}
	if len(plain(frame)) > v.height {
		t.Errorf("el fotograma no debe superar el alto de la terminal")
	}
	st.handle(keypress{code: keyHome}, snap, v)
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "s00.service") {
		t.Error("tras Inicio debe estar elegida la primera fila")
	}
}

func TestProcsView(t *testing.T) {
	snap := testSnapshot()
	v := testView(snap)
	st := newWatchState()
	st.handle(keypress{code: keyRune, r: 'a'}, snap, v)
	st.handle(keypress{code: keyEnter}, snap, v)

	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	list := func(at time.Time, renderer, gpu uint64) ipc.ProcessList {
		return ipc.ProcessList{Time: at, NumCPU: 4, Units: st.target.Units, Processes: []metrics.Process{
			{PID: 100, Name: "chrome", User: "lucas", Threads: 30, CPUTicks: 500, RSSBytes: 400 << 20},
			{PID: 200, Name: "chrome", Role: "renderer", User: "lucas", Threads: 20, CPUTicks: renderer, RSSBytes: 300 << 20},
			{PID: 300, Name: "chrome", Role: "gpu-process", User: "lucas", Threads: 10, CPUTicks: gpu, RSSBytes: 200 << 20},
		}}
	}

	st.updateProcs(list(t0, 1000, 2000), nil)
	frame := strings.Join(plain(st.render(snap, nil, v)), "\n")
	if !strings.Contains(frame, "Midiendo") || !strings.Contains(frame, "Chrome (app) · 3 procesos") {
		t.Errorf("primera lectura:\n%s", frame)
	}

	// En 2 s: el renderer usó 4 s de CPU (400 ticks) = 2 núcleos de 4 = 50 %;
	// la GPU, 80 ticks = 10 %; el principal, nada.
	st.updateProcs(list(t0.Add(2*time.Second), 1400, 2080), nil)
	frame = strings.Join(plain(st.render(snap, nil, v)), "\n")
	for _, want := range []string{"chrome — renderer", "chrome — gpu-process", "50.0", "10.0", "0.0", "Esc volver"} {
		if !strings.Contains(frame, want) {
			t.Errorf("falta %q en:\n%s", want, frame)
		}
	}
	if strings.Index(frame, "renderer") > strings.Index(frame, "gpu-process") {
		t.Error("ordenado por CPU, el renderer (50%) debe ir antes que la GPU (10%)")
	}
	if strings.Contains(frame, "Midiendo") {
		t.Error("tras dos lecturas ya no se está midiendo")
	}

	st.updateProcs(ipc.ProcessList{}, errors.New("la unidad ya no está en ejecución"))
	if frame = strings.Join(plain(st.render(snap, nil, v)), "\n"); !strings.Contains(frame, "ya no está en ejecución") {
		t.Errorf("error de procesos:\n%s", frame)
	}
	if st.handle(keypress{code: keyBack}, snap, v); st.mode != modeUnits {
		t.Error("Retroceso debe volver a la lista")
	}
}

func TestParseKeys(t *testing.T) {
	tests := []struct {
		in   string
		want []keypress
	}{
		{"\x1b", []keypress{{code: keyEsc}}},
		{"\x1b[A\x1b[B", []keypress{{code: keyUp}, {code: keyDown}}},
		{"\x1bOA", []keypress{{code: keyUp}}},
		{"\x1b[5~\x1b[6~", []keypress{{code: keyPageUp}, {code: keyPageDown}}},
		{"\x1b[H\x1b[F", []keypress{{code: keyHome}, {code: keyEnd}}},
		{"\r", []keypress{{code: keyEnter}}},
		{"\x7f", []keypress{{code: keyBack}}},
		{"\x03", []keypress{{code: keyQuit}}},
		{"am", []keypress{{code: keyRune, r: 'a'}, {code: keyRune, r: 'm'}}},
		{"\x1b[1;5A", []keypress{}}, // Ctrl+↑: secuencia desconocida, se ignora
	}
	for _, tt := range tests {
		got := parseKeys([]byte(tt.in))
		if len(got) == 0 && len(tt.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseKeys(%q) = %v, quiero %v", tt.in, got, tt.want)
		}
	}
}

func TestRenderSmallTerminal(t *testing.T) {
	snap := testSnapshot()
	v := view{width: 40, height: 12, color: true, now: snap.Sample.Time}
	st := newWatchState()
	frame := st.render(snap, nil, v)
	if lines := plain(frame); len(lines) > v.height {
		t.Errorf("%d líneas en una terminal de %d", len(lines), v.height)
	}
	if !strings.Contains(frame, ansiRed) || !strings.Contains(frame, ansiReverse) {
		t.Error("con 95% de CPU la barra debería ir en rojo y la fila elegida en vídeo inverso")
	}
}

func TestRenderError(t *testing.T) {
	v := view{width: 80, height: 24, now: time.Now()}
	frame := strings.Join(plain(newWatchState().render(ipc.Snapshot{}, errors.New("conexión rechazada"), v)), "\n")
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
