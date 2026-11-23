package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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
	srv.Register(Command{
		Name: "stats", Usage: "stats [n]", Description: "estadísticas de prueba",
		Text: func([]string) (string, error) { return "texto", nil },
		Data: func(args []string) (any, error) {
			n, err := strconv.Atoi(args[0])
			return map[string]int{"n": n}, err
		},
	})
	srv.Register(Command{Name: "interna", Hidden: true,
		Data: func([]string) (any, error) { return map[string]int{}, nil }})

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
	if err != nil || !resp.OK {
		t.Fatalf("help: %+v, %v", resp, err)
	}
	var infos []ipc.CommandInfo
	if err := resp.Decode(&infos); err != nil {
		t.Fatalf("help sin datos: %v", err)
	}
	var names []string
	for _, in := range infos {
		names = append(names, in.Name)
	}
	if got := strings.Join(names, ","); got != "echo,fail,help,stats" {
		t.Errorf("órdenes en help = %s (la oculta no debe aparecer)", got)
	}
	if !strings.Contains(resp.Output, "stats [n]") || !strings.Contains(resp.Output, "estadísticas de prueba") {
		t.Errorf("la ayuda no muestra uso y descripción:\n%s", resp.Output)
	}

	resp, err = c.Call("stats", "3")
	var got map[string]int
	if err != nil || !resp.OK || resp.Output != "texto" || resp.Decode(&got) != nil || got["n"] != 3 {
		t.Fatalf("orden con texto y datos: %+v, %v, %v", resp, err, got)
	}

	resp, err = c.Call("interna")
	if err != nil || !resp.OK || resp.Decode(&got) != nil {
		t.Fatalf("una orden oculta debe poder llamarse: %+v, %v", resp, err)
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

// Una segunda copia no debe quitarle el socket a un demonio que funciona:
// al cerrarse lo borraría y el primero quedaría inaccesible.
func TestServerRefusesSecondInstance(t *testing.T) {
	c, sock := startServer(t)

	second := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := second.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "ya hay un resguardd") {
		t.Fatalf("la segunda copia debería negarse a arrancar: %v", err)
	}

	// El primero sigue accesible por el mismo socket.
	if resp, err := c.Call("echo", "sigo"); err != nil || resp.Output != "sigo" {
		t.Fatalf("el primer demonio dejó de responder: %+v, %v", resp, err)
	}
	c2, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("el socket del primer demonio desapareció: %v", err)
	}
	c2.Close()
}
