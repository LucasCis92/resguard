package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"

	"github.com/LucasCis92/resguard/internal/ipc"
)

// serviceUnit es la unidad de systemd del demonio.
const serviceUnit = "resguard.service"

// logsArgs construye los argumentos de journalctl para "logs [n] [avisos] [seguir]".
func logsArgs(args []string) ([]string, error) {
	n, follow, warnings := 50, false, false
	for _, a := range args {
		switch a {
		case "seguir", "-f":
			follow = true
		case "avisos":
			warnings = true
		default:
			v, err := strconv.Atoi(a)
			if err != nil || v <= 0 || v > 100000 {
				return nil, fmt.Errorf("argumento desconocido %q (uso: logs [n] [avisos] [seguir])", a)
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

// logs muestra los logs del servicio con journalctl. Durante la ejecución,
// Ctrl+C detiene journalctl pero no la CLI.
func logs(s *session, args []string) error {
	jargs, err := logsArgs(args)
	if err != nil {
		return err
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return errors.New("no se encuentra journalctl: los logs solo están disponibles con systemd")
	}
	if s.socket != ipc.DefaultSocketPath {
		fmt.Fprintln(os.Stderr, "Nota: estos son los logs del servicio instalado; un demonio de desarrollo (make dev) escribe los suyos en su propia terminal.")
	}

	// Se captura SIGINT (no se ignora: una señal ignorada la heredaría
	// journalctl y Ctrl+C dejaría de detenerlo).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	cmd := exec.Command(path, jargs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && !exitErr.Exited() {
		return nil // terminado con Ctrl+C
	}
	return err
}
