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
	"github.com/LucasCis92/resguard/internal/i18n"
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
		pos        int // -1: at the end
		wantLine   string
		wantOpts   []string
	}{
		{"single option", "sta", -1, "status ", nil},
		{"common prefix", "ev", -1, "event", nil},
		{"several, no progress", "event", -1, "event", []string{"event", "events"}},
		{"no match", "zzz", -1, "zzz", nil},
		{"argument", "services m", -1, "services mem ", nil},
		{"possible arguments", "services ", -1, "services ", []string{"cpu", "mem"}},
		{"help completes commands", "help hi", -1, "help history ", nil},
		{"command without known arguments", "status x", -1, "status x", nil},
		{"second argument is not completed", "services cpu 1", -1, "services cpu 1", nil},
		{"cursor in the middle", "st status", 2, "status status", nil},
	}
	c := testCompleter()
	for _, tt := range tests {
		pos := tt.pos
		if pos < 0 {
			pos = len(tt.line)
		}
		line, newPos, opts := c.complete(tt.line, pos)
		if line != tt.wantLine || !reflect.DeepEqual(opts, tt.wantOpts) {
			t.Errorf("%s: complete(%q) = %q, %v; want %q, %v", tt.name, tt.line, line, opts, tt.wantLine, tt.wantOpts)
		}
		if newPos < 0 || newPos > len(line) {
			t.Errorf("%s: position out of range: %d", tt.name, newPos)
		}
	}
}

func TestCompletionArgsFollowLanguage(t *testing.T) {
	if got := completionArgs(i18n.Spanish)["logs"]; !reflect.DeepEqual(got, []string{"avisos", "seguir"}) {
		t.Errorf("Spanish logs arguments = %v", got)
	}
	if got := completionArgs(i18n.English)["services"]; !reflect.DeepEqual(got, []string{"cpu", "mem", "apps", "services", "containers", "sessions"}) {
		t.Errorf("English services arguments = %v", got)
	}
}

func TestHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "resguard", "history")
	h := loadHistory(path, 3)

	for _, e := range []string{"status", "status ", "", "   ", " secret", "events", "services mem ", "history 5"} {
		h.Add(e)
	}
	// No empty lines, no consecutive duplicate, none starting with a space, 3 at most.
	if got := []string{h.At(0), h.At(1), h.At(2)}; h.Len() != 3 || !reflect.DeepEqual(got, []string{"history 5", "services mem", "events"}) {
		t.Fatalf("history = %d %v", h.Len(), got)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("history file mode = %o, want 600", perm)
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %o, want 700", di.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret") {
		t.Error("a line starting with a space must not be saved")
	}

	// Reloading restores it and trims it to the maximum.
	h2 := loadHistory(path, 3)
	if h2.Len() != 3 || h2.At(0) != "history 5" || h2.At(2) != "events" {
		t.Errorf("reloaded history = %v", h2.entries)
	}
	data, _ = os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 3 {
		t.Errorf("the file should be trimmed to 3 lines, it has %d", n)
	}
}

func TestHistoryWithoutFile(t *testing.T) {
	h := loadHistory("", 10)
	h.Add("status")
	if h.Len() != 1 || h.At(0) != "status" {
		t.Errorf("in-memory history = %v", h.entries)
	}
}

func TestTruncateANSI(t *testing.T) {
	colored := "ab" + ansiRed + "cdé" + ansiReset + "fg"
	if got := truncateANSI(colored, 4, true); got != "ab"+ansiRed+"cd"+ansiReset {
		t.Errorf("colored = %q", got)
	}
	if got := truncateANSI("ñandú", 3, false); got != "ñan" {
		t.Errorf("multibyte = %q", got)
	}
	if got := truncateANSI("short", 10, false); got != "short" {
		t.Errorf("not cut = %q", got)
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
			{Name: "a-service-with-a-very-very-long-name-that-does-not-fit.service", Kind: metrics.KindService, CPUPercent: 2},
			{Name: "app-com.google.Chrome-5016.scope", Display: "Chrome", Kind: metrics.KindApp, CPUPercent: 30, MemBytes: 2 << 30},
			{Name: `app-gnome-google\x2dchrome-5016.scope`, Display: "Chrome", Kind: metrics.KindApp, CPUPercent: 5, MemBytes: 1 << 30},
			{Name: "app-gnome-code-6255.scope", Display: "code", Kind: metrics.KindApp, CPUPercent: 20, MemBytes: 3 << 30},
			{Name: "docker-abc.scope", Display: "laravel_app", Kind: metrics.KindContainer, CPUPercent: 0.5, MemBytes: 50 << 20},
		},
		Events: []events.Event{
			{Time: now.Add(-48 * time.Hour), Kind: events.PeakStart, Severity: events.Warning, Resource: "cpu", Value: 91, Threshold: 90},
			{Time: now.Add(-time.Minute), Kind: events.ServiceFailed, Severity: events.Critical, Service: "nginx.service", Reason: "exit-code", ExitStatus: 1},
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

// selectedLine returns the row marked as selected.
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

	for _, want := range []string{"CPU", "95.0%", "load 3.50", "6.0 GiB of 16.0 GiB", "memory 2.5%", "512.0 MiB",
		"(by CPU · all)", "RECENT INCIDENTS", "Enter processes", "laravel_app", "container",
		"nginx.service went down (exited with code 1)", "CPU usage peak: 91.0%"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing %q in:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "\x1b[") {
		t.Error("without color there must be no color sequences")
	}
	// Chrome shows up once, with the usage of its two units added up.
	if n := strings.Count(frame, "Chrome"); n != 1 || !strings.Contains(frame, "35.0") {
		t.Errorf("Chrome should appear once with 35%% CPU:\n%s", frame)
	}
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "busy.service") {
		t.Error("with nothing selected, the marked row must be the first one (busy.service)")
	}
	// The most recent incident goes first; one from another day shows its date.
	if strings.Index(frame, "went down") > strings.Index(frame, "peak:") || !strings.Contains(frame, "10/11 10:00") {
		t.Errorf("wrong incident order or format:\n%s", frame)
	}
	for i, l := range lines {
		if n := utf8.RuneCountInString(l); n > v.width {
			t.Errorf("line %d is %d wide > %d: %q", i, n, v.width, l)
		}
	}

	st.sortBy = "mem"
	frame = strings.Join(plain(st.render(snap, nil, v)), "\n")
	if strings.Index(frame, "code") > strings.Index(frame, "small.service") || !strings.Contains(frame, "(by memory") {
		t.Error("sorted by memory, code (3 GiB) must come before small (900 MiB)")
	}
}

func TestRenderUnitsInSpanish(t *testing.T) {
	snap := testSnapshot()
	v := testView(snap)
	v.p = i18n.New(i18n.Spanish)
	frame := strings.Join(plain(newWatchState().render(snap, nil, v)), "\n")
	for _, want := range []string{"carga 3.50", "6.0 GiB de 16.0 GiB", "memoria 2.5%", "(por CPU · todo)",
		"ÚLTIMAS INCIDENCIAS", "Enter procesos", "contenedor", "NOMBRE", "TIPO",
		"nginx.service se cayó (terminó con código 1)", "pico de uso de CPU: 91.0%", "CRÍTICO"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing %q in:\n%s", want, frame)
		}
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

	// Filter: applications only.
	press(r('a'))
	frame := strings.Join(plain(st.render(snap, nil, v)), "\n")
	if strings.Contains(frame, "busy.service") || strings.Contains(frame, "laravel_app") || !strings.Contains(frame, "only apps") {
		t.Errorf("with the apps filter no services or containers must show:\n%s", frame)
	}
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "Chrome") {
		t.Error("the first app (Chrome, most CPU) should be selected")
	}

	// Move down a row and open its processes.
	press(keypress{code: keyDown})
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "code") {
		t.Error("after ↓ code should be selected")
	}
	press(keypress{code: keyUp})
	if a := press(keypress{code: keyEnter}); a != actionRefresh || st.mode != modeProcs {
		t.Fatalf("Enter must open the process view and ask for data: %v %v", a, st.mode)
	}
	if len(st.target.Units) != 2 {
		t.Errorf("Chrome has two units and the processes of both must be requested: %v", st.target.Units)
	}

	// Esc goes back; the selection and the filter are kept.
	press(keypress{code: keyEsc})
	if st.mode != modeUnits || !strings.Contains(selectedLine(st.render(snap, nil, v)), "Chrome") {
		t.Error("Esc must go back to the list with the same row selected")
	}

	// Containers, then everything.
	press(r('d'))
	frame = strings.Join(plain(st.render(snap, nil, v)), "\n")
	if !strings.Contains(frame, "laravel_app") || strings.Contains(frame, "Chrome") {
		t.Errorf("container filter:\n%s", frame)
	}
	press(r('t'))
	if frame = strings.Join(plain(st.render(snap, nil, v)), "\n"); !strings.Contains(frame, "busy.service") {
		t.Error("t must show everything again")
	}

	if press(r('q')) != actionQuit || press(keypress{code: keyEsc}) != actionQuit {
		t.Error("q and Esc in the list must quit")
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
		t.Errorf("after End the last row must be visible and selected:\n%s", strings.Join(plain(frame), "\n"))
	}
	if len(plain(frame)) > v.height {
		t.Errorf("the frame must not be taller than the terminal")
	}
	st.handle(keypress{code: keyHome}, snap, v)
	if !strings.Contains(selectedLine(st.render(snap, nil, v)), "s00.service") {
		t.Error("after Home the first row must be selected")
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
	if !strings.Contains(frame, "Measuring") || !strings.Contains(frame, "Chrome (app) · 3 processes") {
		t.Errorf("first reading:\n%s", frame)
	}

	// In 2 s the renderer used 4 s of CPU (400 ticks) = 2 of 4 cores = 50 %;
	// the GPU process 80 ticks = 10 %; the main process nothing.
	st.updateProcs(list(t0.Add(2*time.Second), 1400, 2080), nil)
	frame = strings.Join(plain(st.render(snap, nil, v)), "\n")
	for _, want := range []string{"chrome — renderer", "chrome — gpu-process", "50.0", "10.0", "0.0", "Esc back"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing %q in:\n%s", want, frame)
		}
	}
	if strings.Index(frame, "renderer") > strings.Index(frame, "gpu-process") {
		t.Error("sorted by CPU, the renderer (50%) must come before the GPU process (10%)")
	}
	if strings.Contains(frame, "Measuring") {
		t.Error("after two readings it is no longer measuring")
	}

	st.updateProcs(ipc.ProcessList{}, errors.New("the unit is no longer running"))
	if frame = strings.Join(plain(st.render(snap, nil, v)), "\n"); !strings.Contains(frame, "no longer running") {
		t.Errorf("process error:\n%s", frame)
	}
	if st.handle(keypress{code: keyBack}, snap, v); st.mode != modeUnits {
		t.Error("Backspace must go back to the list")
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
		{"\x1b[1;5A", []keypress{}}, // Ctrl+↑: unknown sequence, ignored
	}
	for _, tt := range tests {
		got := parseKeys([]byte(tt.in))
		if len(got) == 0 && len(tt.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseKeys(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRenderSmallTerminal(t *testing.T) {
	snap := testSnapshot()
	v := view{width: 40, height: 12, color: true, now: snap.Sample.Time}
	st := newWatchState()
	frame := st.render(snap, nil, v)
	if lines := plain(frame); len(lines) > v.height {
		t.Errorf("%d lines in a %d-line terminal", len(lines), v.height)
	}
	if !strings.Contains(frame, ansiRed) || !strings.Contains(frame, ansiReverse) {
		t.Error("at 95% CPU the bar should be red and the selected row in reverse video")
	}
}

func TestRenderError(t *testing.T) {
	v := view{width: 80, height: 24, now: time.Now()}
	frame := strings.Join(plain(newWatchState().render(ipc.Snapshot{}, errors.New("connection refused"), v)), "\n")
	if !strings.Contains(frame, "connection refused") || !strings.Contains(frame, "Retrying") {
		t.Errorf("error frame:\n%s", frame)
	}
}

func TestLogsArgs(t *testing.T) {
	p := i18n.New(i18n.English)
	tests := []struct {
		args []string
		want string
	}{
		{nil, "--unit resguard.service --lines 50 --no-hostname --output short --no-pager"},
		{[]string{"200"}, "--unit resguard.service --lines 200 --no-hostname --output short --no-pager"},
		{[]string{"warnings", "follow"}, "--unit resguard.service --lines 50 --no-hostname --output short --priority warning --follow"},
		{[]string{"avisos", "seguir"}, "--unit resguard.service --lines 50 --no-hostname --output short --priority warning --follow"},
		{[]string{"follow", "10"}, "--unit resguard.service --lines 10 --no-hostname --output short --follow"},
	}
	for _, tt := range tests {
		got, err := logsArgs(tt.args, p)
		if err != nil || strings.Join(got, " ") != tt.want {
			t.Errorf("logsArgs(%v) = %v, %v; want %s", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range [][]string{{"all"}, {"-5"}, {"0"}, {"; rm -rf /"}} {
		if _, err := logsArgs(bad, p); err == nil {
			t.Errorf("logsArgs(%v) should fail", bad)
		}
	}
}
