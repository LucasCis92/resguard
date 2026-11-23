package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/LucasCis92/resguard/internal/i18n"
)

const prompt = "resguard> "

// completionArgs returns the values offered by Tab for each command's first
// argument, in the user's language. The daemon accepts both languages.
func completionArgs(lang i18n.Lang) map[string][]string {
	if lang == i18n.Spanish {
		return map[string][]string{
			"services": {"cpu", "mem", "apps", "servicios", "contenedores", "sesiones"},
			"logs":     {"avisos", "seguir"},
		}
	}
	return map[string][]string{
		"services": {"cpu", "mem", "apps", "services", "containers", "sessions"},
		"logs":     {"warnings", "follow"},
	}
}

// repl opens the interactive session. In a terminal it offers history with
// the arrow keys and Tab completion; when input comes from a pipe or a file,
// it reads line by line without editing.
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

	comp := &completer{commands: s.commandNames, args: completionArgs(s.p.Lang())}
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

	fmt.Fprintln(t, s.p.T(`resguardctl: type "help" for the commands and "exit" to quit. Tab completes.`))
	for {
		if w, h, err := term.GetSize(out); err == nil {
			t.SetSize(w, h)
		}
		line, err := t.ReadLine()
		if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
			return // Ctrl+D, Ctrl+C or end of input
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
				fmt.Fprintln(t, s.p.T("error:"), err)
			}
		case "logs":
			// journalctl needs the terminal in normal mode, not raw mode.
			term.Restore(in, oldState)
			if err := logs(s, fields[1:]); err != nil {
				fmt.Fprintln(os.Stderr, s.p.T("error:"), err)
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
			fmt.Println(s.p.T("error:"), s.p.T("watch needs an interactive terminal"))
		case "logs":
			if err := logs(s, fields[1:]); err != nil {
				fmt.Println(s.p.T("error:"), err)
			}
		default:
			s.run(fields, os.Stdout)
		}
	}
}
