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
)

// watch muestra una vista en vivo a pantalla completa hasta que se pulsa q.
// alreadyRaw indica que la terminal ya está en modo raw (dentro del REPL).
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

	v := view{sortBy: "cpu", color: useColor()}
	for {
		snap, err := snapshot(s)
		refresh := interval
		if refresh == 0 {
			refresh = 2 * time.Second
			if err == nil && snap.Interval > 0 {
				refresh = snap.Interval
			}
		}
		v.width, v.height = 80, 24
		if w, h, e := term.GetSize(out); e == nil {
			v.width, v.height = w, h
		}
		v.now, v.refresh = time.Now(), refresh
		os.Stdout.WriteString("\x1b[H" + render(snap, err, v) + "\x1b[J")

		quit, err := waitKeys(in, refresh, &v)
		if quit || err != nil {
			return err
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

// waitKeys espera hasta d atendiendo el teclado. Devuelve quit=true si el
// usuario quiere salir. Usa poll en vez de una goroutine lectora para no
// dejar a nadie leyendo stdin al volver al modo interactivo.
func waitKeys(fd int, d time.Duration, v *view) (quit bool, err error) {
	deadline := time.Now().Add(d)
	buf := make([]byte, 64)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			return false, nil // p. ej. la ventana cambió de tamaño: redibujar
		}
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, nil
		}
		k, err := unix.Read(fd, buf)
		if err != nil || k == 0 {
			return true, nil
		}
		keys := buf[:k]
		if k == 1 && keys[0] == 27 { // Esc solo (las flechas también empiezan por Esc)
			return true, nil
		}
		for _, b := range keys {
			switch b {
			case 'q', 'Q', 3, 4: // q, Ctrl+C, Ctrl+D
				return true, nil
			case 'c', 'C':
				v.sortBy = "cpu"
				return false, nil
			case 'm', 'M':
				v.sortBy = "mem"
				return false, nil
			}
		}
	}
}

// view son las opciones de presentación de un fotograma.
type view struct {
	width, height int
	sortBy        string // "cpu" o "mem"
	color         bool
	now           time.Time
	refresh       time.Duration
}

func useColor() bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && os.Getenv("TERM") != "dumb"
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
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

// render dibuja un fotograma completo. Cada línea se recorta al ancho de la
// terminal y se limpia hasta el final, para redibujar sin parpadeos.
func render(snap ipc.Snapshot, snapErr error, v view) string {
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }

	add("%s   %s · cada %s · q salir · c/m ordenar por CPU/memoria",
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

	// Las incidencias van al pie; los servicios ocupan el espacio que queda.
	evLines := 2 + max(1, len(snap.Events))
	rows := v.height - len(lines) - 2 - evLines
	services := collector.Grouped(snap.Services)
	sort.SliceStable(services, func(i, j int) bool {
		if v.sortBy == "mem" {
			return services[i].MemBytes > services[j].MemBytes
		}
		return services[i].CPUPercent > services[j].CPUPercent
	})

	nameW := 8
	for _, svc := range services {
		nameW = max(nameW, min(32, utf8.RuneCountInString(svc.Label())))
	}
	order := "CPU"
	if v.sortBy == "mem" {
		order = "memoria"
	}
	add("%s", v.paint(ansiBold, fmt.Sprintf("%-*s  %-10s  %6s  %10s  %10s   (por %s)", nameW, "NOMBRE", "TIPO", "CPU%", "MEMORIA", "LÍMITE", order)))
	for i := 0; i < len(services) && i < rows; i++ {
		svc := services[i]
		limit := "-"
		if svc.MemMax > 0 {
			limit = mib(svc.MemMax)
		}
		add("%-*s  %-10s  %6.1f  %10s  %10s", nameW, ellipsis(svc.Label(), nameW), svc.Kind, svc.CPUPercent, mib(svc.MemBytes), limit)
	}
	if len(services) == 0 {
		add("(no se encontraron servicios)")
	}

	add("")
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
