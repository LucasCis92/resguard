package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const prompt = "resguard> "

// repl abre la sesión interactiva. En una terminal ofrece historial con las
// flechas y autocompletado con Tab; si la entrada viene de un pipe o fichero,
// lee línea a línea sin edición.
func repl(s *session) {
	s.interactive = true
	s.loadCommands()

	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(out) {
		plainREPL(s)
		return
	}

	oldState, err := term.MakeRaw(in)
	if err != nil {
		plainREPL(s)
		return
	}
	defer term.Restore(in, oldState)

	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}, prompt)
	t.History = loadHistory(historyPath(), 500)

	comp := &completer{
		commands: s.commandNames,
		args: map[string][]string{
			"services": {"cpu", "mem", "apps", "servicios", "contenedores", "sesiones"},
			"logs":     {"avisos", "seguir"},
		},
	}
	t.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		if key != '\t' {
			return "", 0, false
		}
		newLine, newPos, options := comp.complete(line, pos)
		if len(options) > 0 {
			fmt.Fprintln(t, "  "+strings.Join(options, "   "))
		}
		return newLine, newPos, true
	}

	fmt.Fprintln(t, `resguardctl: escribe "help" para ver las órdenes y "exit" para salir. Tab autocompleta.`)
	for {
		if w, h, err := term.GetSize(out); err == nil {
			t.SetSize(w, h)
		}
		line, err := t.ReadLine()
		if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
			return // Ctrl+D, Ctrl+C o fin de la entrada
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "exit", "quit":
			return
		case "clear":
			fmt.Fprint(os.Stdout, "\x1b[H\x1b[2J")
		case "watch":
			if err := watch(s, fields[1:], true); err != nil {
				fmt.Fprintln(t, "error:", err)
			}
		case "logs":
			// journalctl necesita la terminal en modo normal, no en modo raw.
			term.Restore(in, oldState)
			if err := logs(s, fields[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			if _, err := term.MakeRaw(in); err != nil {
				return
			}
		default:
			s.run(fields, t)
		}
	}
}

func plainREPL(s *session) {
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(prompt)
		if !sc.Scan() {
			fmt.Println()
			return
		}
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "exit", "quit":
			return
		case "watch":
			fmt.Println("error: watch necesita una terminal interactiva")
		case "logs":
			if err := logs(s, fields[1:]); err != nil {
				fmt.Println("error:", err)
			}
		default:
			s.run(fields, os.Stdout)
		}
	}
}
