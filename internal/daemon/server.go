// Package daemon implementa el servidor que atiende a la CLI por socket Unix.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"

	"github.com/LucasCis92/resguard/internal/ipc"
)

// Handler procesa una orden y devuelve el texto a mostrar en la CLI.
type Handler func(args []string) (string, error)

// Server escucha en un socket Unix y despacha órdenes a sus handlers.
type Server struct {
	socketPath string
	group      string
	log        *slog.Logger
	handlers   map[string]Handler
}

// NewServer crea un servidor que escuchará en socketPath.
func NewServer(socketPath string, log *slog.Logger) *Server {
	s := &Server{socketPath: socketPath, log: log, handlers: map[string]Handler{}}
	s.Handle("help", s.help)
	return s
}

// SetSocketGroup hace que el socket pertenezca al grupo indicado, de modo que
// sus miembros puedan usar la CLI sin ser root. Vacío deja el grupo por defecto.
func (s *Server) SetSocketGroup(group string) { s.group = group }

// Handle registra el handler de una orden.
func (s *Server) Handle(command string, h Handler) {
	s.handlers[command] = h
}

// Run escucha hasta que ctx se cancela.
func (s *Server) Run(ctx context.Context) error {
	// Elimina un socket huérfano de una ejecución anterior.
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("eliminando socket previo: %w", err)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("escuchando en %s: %w", s.socketPath, err)
	}
	defer ln.Close()

	if err := os.Chmod(s.socketPath, 0o660); err != nil {
		return fmt.Errorf("ajustando permisos del socket: %w", err)
	}
	if s.group != "" {
		if err := chownGroup(s.socketPath, s.group); err != nil {
			return err
		}
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	s.log.Info("escuchando", "socket", s.socketPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Error("aceptando conexión", "err", err)
			continue
		}
		go s.serve(conn)
	}
}

func chownGroup(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("buscando el grupo del socket: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("gid inválido %q: %w", g.Gid, err)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("asignando el socket al grupo %s: %w", group, err)
	}
	return nil
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	for {
		var req ipc.Request
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Warn("petición inválida", "err", err)
			}
			return
		}

		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			s.log.Warn("enviando respuesta", "err", err)
			return
		}
	}
}

func (s *Server) dispatch(req ipc.Request) ipc.Response {
	h, ok := s.handlers[req.Command]
	if !ok {
		return ipc.Response{Error: fmt.Sprintf("orden desconocida %q (prueba \"help\")", req.Command)}
	}
	out, err := h(req.Args)
	if err != nil {
		return ipc.Response{Error: err.Error()}
	}
	return ipc.Response{OK: true, Output: out}
}

func (s *Server) help([]string) (string, error) {
	names := make([]string, 0, len(s.handlers))
	for name := range s.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return "Órdenes disponibles: " + strings.Join(names, ", "), nil
}
