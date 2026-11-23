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
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
)

// watch muestra una vista en vivo a pantalla completa hasta que se pulsa q.
// Tiene dos modos: la lista de servicios, aplicaciones y contenedores, y los
// procesos de uno de ellos. alreadyRaw indica que la terminal ya está en modo
// raw (dentro del REPL).
func watch(s *session, args []string, alreadyRaw bool) error {
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(out) {
		return errors.New("watch necesita una terminal interactiva")
	}

	var interval time.Duration
	if len(args) > 0 {
		secs, err := strconv.ParseFloat(args[0], 64)
		if err != nil || secs < 0.5 || secs > 3600 {
			return fmt.Errorf("intervalo inválido %q: indica segundos entre 0.5 y 3600", args[0])
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

	// Pantalla alternativa: al salir, la terminal vuelve a estar como antes.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l")
	defer os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")

	st := newWatchState()
	v := view{color: useColor(), refresh: 2 * time.Second}
	var (
		snap    ipc.Snapshot
		snapErr error
		next    time.Time // próxima consulta al demonio
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
					wait = min(wait, time.Second) // la primera medida de CPU necesita dos lecturas
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
				next = time.Time{} // consultar ya (p. ej. al abrir la vista de procesos)
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
	keyBack // Retroceso o flecha izquierda
	keyEsc
	keyQuit // Ctrl+C o Ctrl+D
)

type keypress struct {
	code keyCode
	r    rune
}

// parseKeys interpreta lo leído del teclado en modo raw, incluidas las
// secuencias de escape de las flechas y las teclas de página.
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

// readKeys espera hasta d a que se pulse algo. Usa poll en vez de una
// goroutine lectora para no dejar a nadie leyendo stdin al volver al modo
// interactivo. Devuelve una lista vacía si se agotó el tiempo o la ventana
// cambió de tamaño (en ambos casos se redibuja).
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

// filters son los filtros por tipo de la vista de unidades.
var filters = map[rune]struct{ kind, label string }{
	't': {"", "todo"},
	'a': {metrics.KindApp, "apps"},
	's': {metrics.KindService, "servicios"},
	'd': {metrics.KindContainer, "contenedores"},
}

type watchState struct {
	mode   watchMode
	sortBy string // "cpu" o "mem"
	filter string // tipo de unidad, o "" para todos
	selKey string // clave (tipo + nombre) de la fila elegida
	offset int    // primera fila visible

	// Vista de procesos.
	target    collector.ServiceSample
	procs     ipc.ProcessList
	procsErr  error
	cpu       map[int]float64 // % de CPU por PID; ausente mientras se mide
	prevTicks map[int]uint64
	prevTime  time.Time
	procSort  string
	procOff   int
}

func newWatchState() *watchState {
	return &watchState{sortBy: "cpu", procSort: "cpu"}
}

func unitKey(s collector.ServiceSample) string { return s.Kind + "\x00" + s.Label() }

// units devuelve las filas de la vista de unidades: agrupadas, filtradas y ordenadas.
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

// selected devuelve el índice de la fila elegida (0 si ya no existe).
func (st *watchState) selected(rows []collector.ServiceSample) int {
	for i, r := range rows {
		if unitKey(r) == st.selKey {
			return i
		}
	}
	return 0
}

// handle aplica una tecla.
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

// measuring indica que aún no hay dos lecturas para calcular la CPU.
func (st *watchState) measuring() bool { return st.prevTicks == nil || len(st.cpu) == 0 }

// updateProcs incorpora una lectura de procesos y calcula el % de CPU de cada
// uno comparando su tiempo de CPU con la lectura anterior.
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

// view son las opciones de presentación de un fotograma.
type view struct {
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

// levelColor colorea según la gravedad: verde, amarillo (>=70) o rojo (>=90).
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

// renderUnits dibuja la vista principal. Cada línea se recorta al ancho de la
// terminal y se limpia hasta el final, para redibujar sin parpadeos.
func (st *watchState) renderUnits(snap ipc.Snapshot, snapErr error, v view) string {
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }

	add("%s   %s · cada %s · ↑↓ elegir · Enter procesos · a/s/d/t filtrar · c/m ordenar · q salir",
		v.paint(ansiBold, "resguard"), v.now.Format("15:04:05"), v.refresh)
	add("")

	if snapErr != nil {
		add("%s %v", v.paint(ansiRed, "Sin datos del demonio:"), snapErr)
		add("Reintentando…")
		return finish(lines, v)
	}

	s := snap.Sample
	barW := max(10, min(30, v.width-45))
	add("CPU      %s %5.1f%%   carga %.2f %.2f %.2f", v.bar(s.CPUPercent, barW), s.CPUPercent, s.Load1, s.Load5, s.Load15)
	usedKB := s.MemTotalKB - s.MemAvailableKB
	add("Memoria  %s %5.1f%%   %s de %s", v.bar(s.MemUsedPercent, barW), s.MemUsedPercent, gib(usedKB), gib(s.MemTotalKB))
	add("Swap     %s %5.1f%%", v.bar(s.SwapUsedPercent, barW), s.SwapUsedPercent)
	if s.PSI.Available {
		add("Presión  cpu %.1f%%   memoria %.1f%%   E/S %.1f%%", s.PSI.CPU.Avg10, s.PSI.Memory.Avg10, s.PSI.IO.Avg10)
	} else {
		add("Presión  no disponible en este kernel")
	}
	add("")

	rows := st.units(snap)
	order := "CPU"
	if st.sortBy == "mem" {
		order = "memoria"
	}
	shown := "todo"
	for _, f := range filters {
		if f.kind == st.filter && f.kind != "" {
			shown = "solo " + f.label
		}
	}

	nameW := 8
	for _, r := range rows {
		nameW = max(nameW, min(32, utf8.RuneCountInString(r.Label())))
	}
	add("%s", v.paint(ansiBold, fmt.Sprintf("  %-*s  %-10s  %6s  %10s  %10s   (por %s · %s)",
		nameW, "NOMBRE", "TIPO", "CPU%", "MEMORIA", "LÍMITE", order, shown)))

	// Las incidencias van al pie; las filas ocupan el espacio que queda.
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
		line := fmt.Sprintf("%-*s  %-10s  %6.1f  %10s  %10s", nameW, ellipsis(r.Label(), nameW), r.Kind, r.CPUPercent, mib(r.MemBytes), limit)
		if i == sel {
			// El marcador "›" señala la fila también sin colores.
			add("%s", v.paint(ansiReverse, "› "+line))
		} else {
			add("  %s", line)
		}
	}
	if len(rows) == 0 {
		add("  (nada que mostrar con este filtro; pulsa t para ver todo)")
	}
	for len(lines) < v.height-evLines {
		add("")
	}

	add("%s", v.paint(ansiBold, "ÚLTIMAS INCIDENCIAS"))
	if len(snap.Events) == 0 {
		add("Sin incidencias registradas.")
	}
	for i := len(snap.Events) - 1; i >= 0; i-- {
		e := snap.Events[i]
		add("%s  %s  %s", eventTime(e.Time, v.now), severity(v, e.Severity), e.Message)
	}
	return finish(lines, v)
}

// renderProcs dibuja los procesos de la unidad elegida.
func (st *watchState) renderProcs(snap ipc.Snapshot, v view) string {
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }

	add("%s   %s · cada %s · Esc volver · ↑↓ desplazar · c/m ordenar · q salir",
		v.paint(ansiBold, "resguard"), v.now.Format("15:04:05"), v.refresh)
	add("")

	// Totales de la unidad según el cgroup (la suma de la memoria de los
	// procesos contaría varias veces la compartida).
	cur := st.target
	for _, r := range collector.Grouped(snap.Services) {
		if unitKey(r) == unitKey(st.target) {
			cur = r
		}
	}
	add("%s (%s) · %d procesos · CPU %.1f%% · memoria %s",
		v.paint(ansiBold, cur.Label()), cur.Kind, len(st.procs.Processes), cur.CPUPercent, mib(cur.MemBytes))
	add("")

	if st.procsErr != nil {
		add("%s %v", v.paint(ansiRed, "No se pudieron leer los procesos:"), st.procsErr)
		add("Pulsa Esc para volver.")
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
	order := "CPU"
	if st.procSort == "mem" {
		order = "memoria"
	}
	add("%s", v.paint(ansiBold, fmt.Sprintf("%8s  %-*s  %6s  %10s  %6s  %s   (por %s)",
		"PID", userW, "USUARIO", "CPU%", "MEMORIA", "HILOS", "PROCESO", order)))

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
		add("%8d  %-*s  %6s  %10s  %6d  %s", p.PID, userW, ellipsis(p.User, userW), cpu, mib(p.RSSBytes), p.Threads, name)
	}
	if len(procs) == 0 {
		add("(sin procesos)")
	}
	if rest := len(procs) - (st.procOff + visible); rest > 0 {
		add("… y %d procesos más (↓ para ver)", rest)
	}

	for len(lines) < v.height-1 {
		add("")
	}
	note := "CPU% sobre la capacidad total. La memoria de cada proceso incluye la compartida."
	if st.measuring() {
		note = "Midiendo el uso de CPU de cada proceso…"
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
	switch s {
	case events.Critical:
		return v.paint(ansiRed, "CRÍTICO")
	case events.Warning:
		return v.paint(ansiYellow, "AVISO  ")
	default:
		return "INFO   "
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

// truncateANSI recorta s a width caracteres visibles, sin contar las
// secuencias de escape ANSI ni cortarlas por la mitad.
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
