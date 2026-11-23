package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"

	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
)

// serviceUnit is the daemon's systemd unit.
const serviceUnit = "resguard.service"

// logsArgs builds journalctl's arguments for "logs [n] [warnings] [follow]".
// The Spanish words (avisos, seguir) are accepted too.
func logsArgs(args []string, p i18n.Printer) ([]string, error) {
	n, follow, warnings := 50, false, false
	for _, a := range args {
		switch a {
		case "follow", "seguir", "-f":
			follow = true
		case "warnings", "avisos":
			warnings = true
		default:
			v, err := strconv.Atoi(a)
			if err != nil || v <= 0 || v > 100000 {
				return nil, errors.New(p.T("unknown argument %q (usage: logs [n] [warnings] [follow])", a))
			}
			n = v
		}
	}
	jargs := []string{"--unit", serviceUnit, "--lines", strconv.Itoa(n), "--no-hostname", "--output", "short"}
	if warnings {
		jargs = append(jargs, "--priority", "warning")
	}
	if follow {
		jargs = append(jargs, "--follow")
	} else {
		jargs = append(jargs, "--no-pager")
	}
	return jargs, nil
}

// logs shows the service logs with journalctl. While it runs, Ctrl+C stops
// journalctl but not the CLI.
func logs(s *session, args []string) error {
	jargs, err := logsArgs(args, s.p)
	if err != nil {
		return err
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return errors.New(s.p.T("journalctl not found: logs are only available with systemd"))
	}
	if s.socket != ipc.DefaultSocketPath {
		fmt.Fprintln(os.Stderr, s.p.T("Note: these are the installed service's logs; a development daemon (make dev) writes its own to its terminal."))
	}

	// Catch SIGINT rather than ignore it: an ignored signal would be inherited
	// by journalctl and Ctrl+C would no longer stop it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	cmd := exec.Command(path, jargs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && !exitErr.Exited() {
		return nil // stopped with Ctrl+C
	}
	return err
}
