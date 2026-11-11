package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LucasCis92/resguard/internal/ipc"
)

// startServer levanta un servidor en un socket temporal y devuelve un cliente conectado.
func startServer(t *testing.T) (*ipc.Client, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Handle("echo", func(args []string) (string, error) { return strings.Join(args, " "), nil })
	srv.Handle("fail", func([]string) (string, error) { return "", errors.New("algo salió mal") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run devolvió error: %v", err)
		}
	})

	var client *ipc.Client
	var err error
	for i := 0; i < 100; i++ {
		if client, err = ipc.Dial(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no se pudo conectar: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, sock
}

func TestServerRoundTrip(t *testing.T) {
	c, sock := startServer(t)

	// Varias órdenes por la misma conexión.
	resp, err := c.Call("echo", "hola", "mundo")
	if err != nil || !resp.OK || resp.Output != "hola mundo" {
		t.Fatalf("echo: %+v, %v", resp, err)
	}

	resp, err = c.Call("fail")
	if err != nil || resp.OK || resp.Error != "algo salió mal" {
		t.Fatalf("fail: %+v, %v", resp, err)
	}

	resp, err = c.Call("noexiste")
	if err != nil || resp.OK || !strings.Contains(resp.Error, "desconocida") {
		t.Fatalf("orden desconocida: %+v, %v", resp, err)
	}

	resp, err = c.Call("help")
	if err != nil || !strings.Contains(resp.Output, "echo, fail, help") {
		t.Fatalf("help: %+v, %v", resp, err)
	}

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o660 {
		t.Errorf("permisos del socket = %o, quiero 660", perm)
	}
}

func TestServerReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "stale.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	var err error
	for i := 0; i < 100; i++ {
		var c *ipc.Client
		if c, err = ipc.Dial(sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if runErr := <-done; runErr != nil {
		t.Fatal(runErr)
	}
	if err != nil {
		t.Fatalf("no reemplazó el socket huérfano: %v", err)
	}
}

func TestServerUnknownGroup(t *testing.T) {
	srv := NewServer(filepath.Join(t.TempDir(), "g.sock"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.SetSocketGroup("grupo-que-no-existe-resguard")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Run(ctx); err == nil {
		t.Fatal("un grupo inexistente debería impedir arrancar")
	}
}
