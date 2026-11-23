package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
)

// watch shows a full-screen live view until q is pressed. It has two modes:
// the list of services, applications and containers, and the processes of
// one of them. alreadyRaw says the terminal is already in raw mode (inside
// the REPL).
func watch(s *session, args []string, alreadyRaw bool) error {
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(out) {
		return errors.New(s.p.T("watch needs an interactive terminal"))
	}

	var interval time.Duration
	if len(args) > 0 {
		secs, err := strconv.ParseFloat(args[0], 64)
		if err != nil || secs < 0.5 || secs > 3600 {
			return errors.New(s.p.T("invalid interval %q: give seconds between 0.5 and 3600", args[0]))
		}
		interval = time.Duration(secs * float64(time.Second))
	}

	if !alreadyRaw {
		old, err := term.MakeRaw(in)
		if err != nil {
			return err
		}
		defer term.Restore(in, old)
	}

	// Alternate screen: on exit, the terminal is left as it was.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l")
	defer os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")

	st := newWatchState()
	v := view{color: useColor(), refresh: 2 * time.Second, p: s.p}
	var (
		snap    ipc.Snapshot
		snapErr error
		next    time.Time // next query to the daemon
	)
	for {
		if !time.Now().Before(next) {
			snap, snapErr = snapshot(s)
			v.refresh = interval
			if v.refresh == 0 {
				v.refresh = 2 * time.Second
				if snapErr == nil && snap.Interval > 0 {
					v.refresh = snap.Interval
				}
			}
			wait := v.refresh
			if st.mode == modeProcs {
				st.updateProcs(fetchProcesses(s, st.target.Units))
				if st.measuring() {
					wait = min(wait, time.Second) // the first CPU measurement needs two readings
				}
			}
			next = time.Now().Add(wait)
		}

		v.width, v.height = 80, 24
		if w, h, e := term.GetSize(out); e == nil {
			v.width, v.height = w, h
		}
		v.now = time.Now()
		os.Stdout.WriteString("\x1b[H" + st.render(snap, snapErr, v) + "\x1b[J")

		keys, err := readKeys(in, time.Until(next))
		if err != nil {
			return err
		}
		for _, k := range keys {
			switch st.handle(k, snap, v) {
			case actionQuit:
				return nil
			case actionRefresh:
				next = time.Time{} // query now (e.g. when opening the process view)
			}
		}
	}
}

func snapshot(s *session) (ipc.Snapshot, error) {
	var snap ipc.Snapshot
	resp, err := s.call("snapshot")
	if err != nil {
		return snap, err
	}
	if !resp.OK {
		return snap, errors.New(resp.Error)
	}
	return snap, resp.Decode(&snap)
}

func fetchProcesses(s *session, units []string) (ipc.ProcessList, error) {
	var list ipc.ProcessList
	resp, err := s.call("processes", units...)
	if err != nil {
		return list, err
	}
	if !resp.OK {
		return list, errors.New(resp.Error)
	}
	return list, resp.Decode(&list)
}

// ---------------------------------------------------------------- teclado

type keyCode int

const (
	keyRune keyCode = iota
	keyUp
	keyDown
	keyPageUp
	keyPageDown
	keyHome
	keyEnd
	keyEnter
	keyBack // Backspace or left arrow
	keyEsc
	keyQuit // Ctrl+C or Ctrl+D
)

type keypress struct {
	code keyCode
	r    rune
}

// parseKeys decodes keyboard input read in raw mode, including the escape
// sequences of the arrow and page keys.
func parseKeys(b []byte) []keypress {
	if len(b) == 1 && b[0] == 27 {
		return []keypress{{code: keyEsc}}
	}
	var out []keypress
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 27 && i+1 < len(b) && (b[i+1] == '[' || b[i+1] == 'O'):
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j >= len(b) {
				return out
			}
			seq := string(b[i+2 : j+1])
			switch seq {
			case "A":
				out = append(out, keypress{code: keyUp})
			case "B":
				out = append(out, keypress{code: keyDown})
			case "D":
				out = append(out, keypress{code: keyBack})
			case "C":
				out = append(out, keypress{code: keyEnter})
			case "5~":
				out = append(out, keypress{code: keyPageUp})
			case "6~":
				out = append(out, keypress{code: keyPageDown})
			case "H", "1~":
				out = append(out, keypress{code: keyHome})
			case "F", "4~":
				out = append(out, keypress{code: keyEnd})
			}
			i = j
		case c == 27:
			out = append(out, keypress{code: keyEsc})
		case c == '\r' || c == '\n':
			out = append(out, keypress{code: keyEnter})
		case c == 127 || c == 8:
			out = append(out, keypress{code: keyBack})
		case c == 3 || c == 4:
			out = append(out, keypress{code: keyQuit})
		default:
			out = append(out, keypress{code: keyRune, r: rune(c)})
		}
	}
	return out
}

// readKeys waits up to d for a key press. It uses poll instead of a reader
// goroutine so that nothing is left reading stdin when going back to
// interactive mode. It returns an empty list when the time runs out or the
// window is resized (both cases redraw).
func readKeys(fd int, d time.Duration) ([]keypress, error) {
	if d <= 0 {
		return nil, nil
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(d.Milliseconds())+1)
	if errors.Is(err, unix.EINTR) || n == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 64)
	k, err := unix.Read(fd, buf)
	if err != nil || k == 0 {
		return []keypress{{code: keyQuit}}, nil
	}
	return parseKeys(buf[:k]), nil
}

// ---------------------------------------------------------------- estado

type watchMode int

const (
	modeUnits watchMode = iota
	modeProcs
)

type action int

const (
	actionNone action = iota
	actionQuit
	actionRefresh
)

// filters are the kind filters of the unit list.
var filters = map[rune]struct{ kind, label string }{
	't': {"", i18n.M("all")},
	'a': {metrics.KindApp, i18n.M("apps")},
	's': {metrics.KindService, i18n.M("services")},
	'd': {metrics.KindContainer, i18n.M("containers")},
}

type watchState struct {
	mode   watchMode
	sortBy string // "cpu" or "mem"
	filter string // unit kind, or "" for all
	selKey string // key (kind + name) of the selected row
	offset int    // first visible row

	// Process view.
	target    collector.ServiceSample
	procs     ipc.ProcessList
	procsErr  error
	cpu       map[int]float64 // CPU % per PID; missing while measuring
	prevTicks map[int]uint64
	prevTime  time.Time
	procSort  string
	procOff   int
}

func newWatchState() *watchState {
	return &watchState{sortBy: "cpu", procSort: "cpu"}
}

func unitKey(s collector.ServiceSample) string { return s.Kind + "\x00" + s.Label() }

// units returns the rows of the unit list: grouped, filtered and sorted.
func (st *watchState) units(snap ipc.Snapshot) []collector.ServiceSample {
	var out []collector.ServiceSample
	for _, s := range collector.Grouped(snap.Services) {
		if st.filter == "" || s.Kind == st.filter {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if st.sortBy == "mem" {
			return out[i].MemBytes > out[j].MemBytes
		}
		return out[i].CPUPercent > out[j].CPUPercent
	})
	return out
}

// selected returns the index of the selected row (0 if it no longer exists).
func (st *watchState) selected(rows []collector.ServiceSample) int {
	for i, r := range rows {
		if unitKey(r) == st.selKey {
			return i
		}
	}
	return 0
}

// handle applies a key press.
func (st *watchState) handle(k keypress, snap ipc.Snapshot, v view) action {
	if k.code == keyQuit || k.code == keyRune && (k.r == 'q' || k.r == 'Q') {
		return actionQuit
	}
	if st.mode == modeProcs {
		return st.handleProcs(k, v)
	}

	rows := st.units(snap)
	sel := st.selected(rows)
	page := max(1, v.height-14)
	move := func(to int) {
		if len(rows) > 0 {
			st.selKey = unitKey(rows[max(0, min(len(rows)-1, to))])
		}
	}
	switch k.code {
	case keyEsc:
		return actionQuit
	case keyUp:
		move(sel - 1)
	case keyDown:
		move(sel + 1)
	case keyPageUp:
		move(sel - page)
	case keyPageDown:
		move(sel + page)
	case keyHome:
		move(0)
	case keyEnd:
		move(len(rows) - 1)
	case keyEnter:
		if len(rows) == 0 {
			return actionNone
		}
		st.openProcs(rows[sel])
		return actionRefresh
	case keyRune:
		switch k.r {
		case 'c', 'C':
			st.sortBy = "cpu"
		case 'm', 'M':
			st.sortBy = "mem"
		case 'k':
			move(sel - 1)
		case 'j':
			move(sel + 1)
		default:
			if f, ok := filters[k.r]; ok {
				st.filter = f.kind
				st.offset = 0
			}
		}
	}
	return actionNone
}

func (st *watchState) handleProcs(k keypress, v view) action {
	page := max(1, v.height-10)
	switch k.code {
	case keyEsc, keyBack:
		st.mode = modeUnits
	case keyUp:
		st.procOff = max(0, st.procOff-1)
	case keyDown:
		st.procOff++
	case keyPageUp:
		st.procOff = max(0, st.procOff-page)
	case keyPageDown:
		st.procOff += page
	case keyHome:
		st.procOff = 0
	case keyRune:
		switch k.r {
		case 'c', 'C':
			st.procSort = "cpu"
		case 'm', 'M':
			st.procSort = "mem"
		case 'k':
			st.procOff = max(0, st.procOff-1)
		case 'j':
			st.procOff++
		}
	}
	return actionNone
}

func (st *watchState) openProcs(target collector.ServiceSample) {
	st.mode = modeProcs
	st.target = target
	st.selKey = unitKey(target)
	st.procs, st.procsErr = ipc.ProcessList{}, nil
	st.cpu, st.prevTicks, st.prevTime = map[int]float64{}, nil, time.Time{}
	st.procOff = 0
}

// measuring reports that there are not yet two readings to compute CPU usage.
func (st *watchState) measuring() bool { return st.prevTicks == nil || len(st.cpu) == 0 }

// updateProcs takes in a process reading and computes each process's CPU %
// by comparing its CPU time with the previous reading.
func (st *watchState) updateProcs(list ipc.ProcessList, err error) {
	st.procsErr = err
	if err != nil {
		return
	}
	st.procs = list
	ticks := make(map[int]uint64, len(list.Processes))
	cpu := make(map[int]float64, len(list.Processes))
	elapsed := list.Time.Sub(st.prevTime).Seconds()
	ncpu := float64(max(1, list.NumCPU))
	for _, p := range list.Processes {
		ticks[p.PID] = p.CPUTicks
		if prev, ok := st.prevTicks[p.PID]; ok && elapsed > 0 && p.CPUTicks >= prev {
			cpu[p.PID] = 100 * float64(p.CPUTicks-prev) / metrics.ClockTicks / elapsed / ncpu
		}
	}
	st.prevTicks, st.prevTime, st.cpu = ticks, list.Time, cpu
}

// ---------------------------------------------------------------- dibujo

// view holds the presentation options of a frame.
type view struct {
	p             i18n.Printer
	width, height int
	color         bool
	now           time.Time
	refresh       time.Duration
}

func useColor() bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && os.Getenv("TERM") != "dumb"
}

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiReverse = "\x1b[7m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
)

func (v view) paint(code, s string) string {
	if !v.color {
		return s
	}
	return code + s + ansiReset
}

// levelColor picks a color by level: green, yellow (>=70) or red (>=90).
func levelColor(pct float64) string {
	switch {
	case pct >= 90:
		return ansiRed
	case pct >= 70:
		return ansiYellow
	default:
		return ansiGreen
	}
}

func (v view) bar(pct float64, width int) string {
	filled := int(pct/100*float64(width) + 0.5)
	filled = max(0, min(width, filled))
	return "[" + v.paint(levelColor(pct), strings.Repeat("█", filled)) + strings.Repeat("░", width-filled) + "]"
}

func (st *watchState) render(snap ipc.Snapshot, snapErr error, v view) string {
	if st.mode == modeProcs {
		return st.renderProcs(snap, v)
	}
	return st.renderUnits(snap, snapErr, v)
}

// renderUnits draws the main view. Every line is cut to the terminal width
// and cleared to its end, to redraw without flicker.
func (st *watchState) renderUnits(snap ipc.Snapshot, snapErr error, v view) string {
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }

	add("%s   %s", v.paint(ansiBold, "resguard"),
		v.p.T("%s · every %s · ↑↓ select · Enter processes · a/s/d/t filter · c/m sort · q quit", v.now.Format("15:04:05"), v.refresh))
	add("")

	if snapErr != nil {
		add("%s %v", v.paint(ansiRed, v.p.T("No data from the daemon:")), snapErr)
		add("%s", v.p.T("Retrying…"))
		return finish(lines, v)
	}

	s := snap.Sample
	barW := max(10, min(30, v.width-45))
	label := func(s string) string { return fmt.Sprintf("%-9s", s) }
	add("%s%s %5.1f%%   %s", label(v.p.T("CPU")), v.bar(s.CPUPercent, barW), s.CPUPercent, v.p.T("load %.2f %.2f %.2f", s.Load1, s.Load5, s.Load15))
	usedKB := s.MemTotalKB - s.MemAvailableKB
	add("%s%s %5.1f%%   %s", label(v.p.T("Memory")), v.bar(s.MemUsedPercent, barW), s.MemUsedPercent, v.p.T("%s of %s", gib(usedKB), gib(s.MemTotalKB)))
	add("%s%s %5.1f%%", label(v.p.T("Swap")), v.bar(s.SwapUsedPercent, barW), s.SwapUsedPercent)
	if s.PSI.Available {
		add("%s%s", label(v.p.T("Pressure")), v.p.T("cpu %.1f%%   memory %.1f%%   I/O %.1f%%", s.PSI.CPU.Avg10, s.PSI.Memory.Avg10, s.PSI.IO.Avg10))
	} else {
		add("%s%s", label(v.p.T("Pressure")), v.p.T("not available on this kernel"))
	}
	add("")

	rows := st.units(snap)
	order := v.p.T("by CPU")
	if st.sortBy == "mem" {
		order = v.p.T("by memory")
	}
	shown := v.p.T("all")
	for _, f := range filters {
		if f.kind == st.filter && f.kind != "" {
			shown = v.p.T("only %s", v.p.T(f.label))
		}
	}

	nameW := 8
	for _, r := range rows {
		nameW = max(nameW, min(32, utf8.RuneCountInString(r.Label())))
	}
	add("%s", v.paint(ansiBold, fmt.Sprintf("  %-*s  %-10s  %6s  %10s  %10s   (%s · %s)",
		nameW, v.p.T("NAME"), v.p.T("KIND"), "CPU%", v.p.T("MEMORY"), v.p.T("LIMIT"), order, shown)))

	// Incidents go at the bottom; the rows take the remaining space.
	evLines := 2 + max(1, len(snap.Events))
	visible := max(1, v.height-len(lines)-1-evLines)
	sel := st.selected(rows)
	if sel < st.offset {
		st.offset = sel
	}
	if sel >= st.offset+visible {
		st.offset = sel - visible + 1
	}
	st.offset = max(0, min(st.offset, max(0, len(rows)-visible)))

	for i := st.offset; i < len(rows) && i < st.offset+visible; i++ {
		r := rows[i]
		limit := "-"
		if r.MemMax > 0 {
			limit = mib(r.MemMax)
		}
		line := fmt.Sprintf("%-*s  %-10s  %6.1f  %10s  %10s", nameW, ellipsis(r.Label(), nameW), events.KindLabel(r.Kind, v.p), r.CPUPercent, mib(r.MemBytes), limit)
		if i == sel {
			// The "›" marker shows the row even without colors.
			add("%s", v.paint(ansiReverse, "› "+line))
		} else {
			add("  %s", line)
		}
	}
	if len(rows) == 0 {
		add("  %s", v.p.T("(nothing to show with this filter; press t to see everything)"))
	}
	for len(lines) < v.height-evLines {
		add("")
	}

	add("%s", v.paint(ansiBold, v.p.T("RECENT INCIDENTS")))
	if len(snap.Events) == 0 {
		add("%s", v.p.T("No incidents recorded."))
	}
	for i := len(snap.Events) - 1; i >= 0; i-- {
		e := snap.Events[i]
		add("%s  %s  %s", eventTime(e.Time, v.now), severity(v, e.Severity), events.Describe(e, v.p))
	}
	return finish(lines, v)
}

// renderProcs draws the processes of the selected unit.
func (st *watchState) renderProcs(snap ipc.Snapshot, v view) string {
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }

	add("%s   %s", v.paint(ansiBold, "resguard"),
		v.p.T("%s · every %s · Esc back · ↑↓ scroll · c/m sort · q quit", v.now.Format("15:04:05"), v.refresh))
	add("")

	// Unit totals come from the cgroup (adding up the processes' memory would
	// count shared memory several times).
	cur := st.target
	for _, r := range collector.Grouped(snap.Services) {
		if unitKey(r) == unitKey(st.target) {
			cur = r
		}
	}
	add("%s %s", v.paint(ansiBold, cur.Label()), v.p.T("(%s) · %d processes · CPU %.1f%% · memory %s",
		events.KindLabel(cur.Kind, v.p), len(st.procs.Processes), cur.CPUPercent, mib(cur.MemBytes)))
	add("")

	if st.procsErr != nil {
		add("%s %v", v.paint(ansiRed, v.p.T("Could not read the processes:")), st.procsErr)
		add("%s", v.p.T("Press Esc to go back."))
		return finish(lines, v)
	}

	procs := append([]metrics.Process(nil), st.procs.Processes...)
	sort.SliceStable(procs, func(i, j int) bool {
		if st.procSort == "mem" {
			return procs[i].RSSBytes > procs[j].RSSBytes
		}
		ci, cj := st.cpu[procs[i].PID], st.cpu[procs[j].PID]
		if ci != cj {
			return ci > cj
		}
		return procs[i].RSSBytes > procs[j].RSSBytes
	})

	userW := 7
	for _, p := range procs {
		userW = max(userW, min(16, utf8.RuneCountInString(p.User)))
	}
	order := v.p.T("by CPU")
	if st.procSort == "mem" {
		order = v.p.T("by memory")
	}
	add("%s", v.paint(ansiBold, fmt.Sprintf("%8s  %-*s  %6s  %10s  %7s  %s   (%s)",
		"PID", userW, v.p.T("USER"), "CPU%", v.p.T("MEMORY"), v.p.T("THREADS"), v.p.T("PROCESS"), order)))

	visible := max(1, v.height-len(lines)-2)
	st.procOff = max(0, min(st.procOff, max(0, len(procs)-visible)))
	for i := st.procOff; i < len(procs) && i < st.procOff+visible; i++ {
		p := procs[i]
		cpu := "…"
		if c, ok := st.cpu[p.PID]; ok {
			cpu = fmt.Sprintf("%.1f", c)
		}
		name := p.Name
		if p.Role != "" {
			name += " — " + p.Role
		}
		add("%8d  %-*s  %6s  %10s  %7d  %s", p.PID, userW, ellipsis(p.User, userW), cpu, mib(p.RSSBytes), p.Threads, name)
	}
	if len(procs) == 0 {
		add("%s", v.p.T("(no processes)"))
	}
	if rest := len(procs) - (st.procOff + visible); rest > 0 {
		add("%s", v.p.T("… and %d more processes (↓ to see them)", rest))
	}

	for len(lines) < v.height-1 {
		add("")
	}
	note := v.p.T("CPU usage is relative to the whole machine. Each process's memory includes shared memory.")
	if st.measuring() {
		note = v.p.T("Measuring each process's CPU usage…")
	}
	add("%s", note)
	return finish(lines, v)
}

func finish(lines []string, v view) string {
	if len(lines) > v.height {
		lines = lines[:v.height]
	}
	for i, l := range lines {
		lines[i] = truncateANSI(l, v.width, v.color) + "\x1b[K"
	}
	return strings.Join(lines, "\r\n")
}

func severity(v view, s events.Severity) string {
	label := fmt.Sprintf("%-8s", events.SeverityLabel(s, v.p))
	switch s {
	case events.Critical:
		return v.paint(ansiRed, label)
	case events.Warning:
		return v.paint(ansiYellow, label)
	default:
		return label
	}
}

func eventTime(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return t.Format("15:04:05")
	}
	return t.Format("02/01 15:04")
}

// truncateANSI cuts s to width visible characters, without counting ANSI
// escape sequences or cutting them in half.
func truncateANSI(s string, width int, colored bool) string {
	visible := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			i = j + 1
			continue
		}
		if visible == width {
			if colored {
				return s[:i] + ansiReset
			}
			return s[:i]
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		visible++
	}
	return s
}

func ellipsis(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	r := []rune(s)
	return string(r[:width-1]) + "…"
}

func mib(b uint64) string  { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
func gib(kb uint64) string { return fmt.Sprintf("%.1f GiB", float64(kb)/(1<<20)) }
