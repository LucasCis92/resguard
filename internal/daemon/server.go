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
	"text/tabwriter"

	"github.com/LucasCis92/resguard/internal/ipc"
)

// Handler procesa una orden y devuelve el texto a mostrar en la CLI.
type Handler func(args []string) (string, error)

// Command describe una orden del demonio. Debe tener Text, Data o ambos.
type Command struct {
	Name        string
	Usage       string // p. ej. "services [cpu|mem] [n]"
	Description string
	Hidden      bool // no aparece en "help" (órdenes internas de la CLI)

	Text func(args []string) (string, error) // salida en texto
	Data func(args []string) (any, error)    // salida estructurada
}

// Server escucha en un socket Unix y despacha órdenes a sus handlers.
type Server struct {
	socketPath string
	group      string
	log        *slog.Logger
	commands   map[string]Command
}

// NewServer crea un servidor que escuchará en socketPath.
func NewServer(socketPath string, log *slog.Logger) *Server {
	s := &Server{socketPath: socketPath, log: log, commands: map[string]Command{}}
	s.Register(Command{Name: "help", Description: "muestra esta ayuda"})
	return s
}

// SetSocketGroup hace que el socket pertenezca al grupo indicado, de modo que
// sus miembros puedan usar la CLI sin ser root. Vacío deja el grupo por defecto.
func (s *Server) SetSocketGroup(group string) { s.group = group }

// Register registra una orden.
func (s *Server) Register(c Command) { s.commands[c.Name] = c }

// Handle registra una orden de texto sin descripción.
func (s *Server) Handle(name string, h Handler) { s.Register(Command{Name: name, Text: h}) }

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
	if req.Command == "help" {
		return s.help()
	}
	c, ok := s.commands[req.Command]
	if !ok {
		return ipc.Response{Error: fmt.Sprintf("orden desconocida %q (prueba \"help\")", req.Command)}
	}

	var resp ipc.Response
	if c.Text != nil {
		out, err := c.Text(req.Args)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		resp.Output = out
	}
	if c.Data != nil {
		v, err := c.Data(req.Args)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		data, err := json.Marshal(v)
		if err != nil {
			s.log.Error("codificando respuesta", "command", c.Name, "err", err)
			return ipc.Response{Error: "error interno codificando la respuesta"}
		}
		resp.Data = data
	}
	resp.OK = true
	return resp
}

// help devuelve la lista de órdenes como texto y como datos (para autocompletar).
func (s *Server) help() ipc.Response {
	var infos []ipc.CommandInfo
	for _, c := range s.commands {
		if c.Hidden {
			continue
		}
		usage := c.Usage
		if usage == "" {
			usage = c.Name
		}
		infos = append(infos, ipc.CommandInfo{Name: c.Name, Usage: usage, Description: c.Description})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	for _, in := range infos {
		fmt.Fprintf(tw, "  %s\t%s\n", in.Usage, in.Description)
	}
	tw.Flush()

	data, _ := json.Marshal(infos)
	return ipc.Response{OK: true, Output: "Órdenes del demonio:\n" + strings.TrimRight(b.String(), "\n"), Data: data}
}
