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

// startServer starts a server on a temporary socket and returns a connected client.
func startServer(t *testing.T) (*ipc.Client, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Handle("echo", func(c Call) (string, error) { return strings.Join(c.Args, " "), nil })
	srv.Handle("fail", func(Call) (string, error) { return "", errors.New("something went wrong") })
	srv.Register(Command{
		Name: "stats", Usage: "stats [n]", Description: "test statistics",
		Text: func(Call) (string, error) { return "text", nil },
		Data: func(c Call) (any, error) {
			n, err := strconv.Atoi(c.Args[0])
			return map[string]int{"n": n}, err
		},
	})
	srv.Register(Command{Name: "internal", Hidden: true,
		Data: func(Call) (any, error) { return map[string]int{}, nil }})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned an error: %v", err)
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
		t.Fatalf("could not connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, sock
}

func TestServerRoundTrip(t *testing.T) {
	c, sock := startServer(t)

	// Several commands over the same connection.
	resp, err := c.Call("echo", "hola", "mundo")
	if err != nil || !resp.OK || resp.Output != "hola mundo" {
		t.Fatalf("echo: %+v, %v", resp, err)
	}

	resp, err = c.Call("fail")
	if err != nil || resp.OK || resp.Error != "something went wrong" {
		t.Fatalf("fail: %+v, %v", resp, err)
	}

	resp, err = c.Call("nonexistent")
	if err != nil || resp.OK || !strings.Contains(resp.Error, "unknown command") {
		t.Fatalf("unknown command: %+v, %v", resp, err)
	}

	resp, err = c.Call("help")
	if err != nil || !resp.OK {
		t.Fatalf("help: %+v, %v", resp, err)
	}
	var infos []ipc.CommandInfo
	if err := resp.Decode(&infos); err != nil {
		t.Fatalf("help without data: %v", err)
	}
	var names []string
	for _, in := range infos {
		names = append(names, in.Name)
	}
	if got := strings.Join(names, ","); got != "echo,fail,help,stats" {
		t.Errorf("commands in help = %s (the hidden one must not show up)", got)
	}
	if !strings.Contains(resp.Output, "stats [n]") || !strings.Contains(resp.Output, "test statistics") {
		t.Errorf("help does not show usage and description:\n%s", resp.Output)
	}

	resp, err = c.Call("stats", "3")
	var got map[string]int
	if err != nil || !resp.OK || resp.Output != "text" || resp.Decode(&got) != nil || got["n"] != 3 {
		t.Fatalf("command with text and data: %+v, %v, %v", resp, err, got)
	}

	resp, err = c.Call("internal")
	if err != nil || !resp.OK || resp.Decode(&got) != nil {
		t.Fatalf("a hidden command must be callable: %+v, %v", resp, err)
	}

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o660 {
		t.Errorf("socket permissions = %o, want 660", perm)
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
		t.Fatalf("the stale socket was not replaced: %v", err)
	}
}

func TestServerUnknownGroup(t *testing.T) {
	srv := NewServer(filepath.Join(t.TempDir(), "g.sock"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.SetSocketGroup("resguard-missing-group")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Run(ctx); err == nil {
		t.Fatal("a missing group should prevent startup")
	}
}

// A second copy must not take the socket from a running daemon: on exit it
// would delete it and the first one would become unreachable.
func TestServerRefusesSecondInstance(t *testing.T) {
	c, sock := startServer(t)

	second := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := second.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the second copy should refuse to start: %v", err)
	}

	// The first one is still reachable through the same socket.
	if resp, err := c.Call("echo", "sigo"); err != nil || resp.Output != "sigo" {
		t.Fatalf("the first daemon stopped answering: %+v, %v", resp, err)
	}
	c2, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("the first daemon's socket disappeared: %v", err)
	}
	c2.Close()
}

// The daemon answers in the language each request asks for.
func TestServerAnswersInTheCallersLanguage(t *testing.T) {
	c, _ := startServer(t)
	c.Lang = "es"
	resp, err := c.Call("nonexistent")
	if err != nil || !strings.Contains(resp.Error, "orden desconocida") {
		t.Errorf("Spanish error = %+v, %v", resp, err)
	}
	resp, _ = c.Call("help")
	if !strings.HasPrefix(resp.Output, "Órdenes del demonio:") || !strings.Contains(resp.Output, "muestra esta ayuda") {
		t.Errorf("Spanish help:\n%s", resp.Output)
	}
	c.Lang = "en"
	if resp, _ = c.Call("help"); !strings.HasPrefix(resp.Output, "Daemon commands:") {
		t.Errorf("English help:\n%s", resp.Output)
	}
}
