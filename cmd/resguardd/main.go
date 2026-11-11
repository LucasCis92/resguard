// resguardd es el demonio que vigila los recursos del sistema y de los servicios.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/LucasCis92/resguard/internal/daemon"
	"github.com/LucasCis92/resguard/internal/ipc"
	"github.com/LucasCis92/resguard/internal/metrics"
	"github.com/LucasCis92/resguard/internal/version"
)

func main() {
	socketPath := flag.String("socket", ipc.DefaultSocketPath, "ruta del socket Unix")
	flag.Parse()

	// Bajo systemd, stderr va al journal.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := daemon.NewServer(*socketPath, log)
	srv.Handle("ping", func([]string) (string, error) { return "pong", nil })
	srv.Handle("version", func([]string) (string, error) { return version.Version, nil })
	srv.Handle("status", status)

	log.Info("iniciando resguardd", "version", version.Version)
	if err := srv.Run(ctx); err != nil {
		log.Error("el demonio terminó con error", "err", err)
		os.Exit(1)
	}
	log.Info("resguardd detenido")
}

func status([]string) (string, error) {
	s, err := metrics.Read()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Carga: %.2f %.2f %.2f\nMemoria: %.1f%% usada (%d MiB disponibles de %d MiB)",
		s.Load1, s.Load5, s.Load15,
		s.MemUsedPercent(), s.MemAvailableKB/1024, s.MemTotalKB/1024,
	), nil
}
