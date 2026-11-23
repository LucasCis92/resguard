// Package daemon implements the server that answers the CLI over a Unix socket.
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
	"time"

	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
)

// Call is one invocation of a command: its arguments and a printer for the
// caller's language.
type Call struct {
	Args []string
	P    i18n.Printer
}

// Handler processes a command and returns the text to show in the CLI.
type Handler func(c Call) (string, error)

// Command describes a daemon command. It must have Text, Data or both.
// Usage and Description are English and are translated when shown, so mark
// them with i18n.M.
type Command struct {
	Name        string
	Usage       string // e.g. "services [cpu|mem] [n]"
	Description string
	Hidden      bool // not listed in "help" (commands used internally by the CLI)

	Text func(c Call) (string, error) // text output
	Data func(c Call) (any, error)    // structured output
}

// Server listens on a Unix socket and dispatches commands to their handlers.
type Server struct {
	socketPath string
	group      string
	log        *slog.Logger
	commands   map[string]Command
}

// NewServer creates a server that will listen on socketPath.
func NewServer(socketPath string, log *slog.Logger) *Server {
	s := &Server{socketPath: socketPath, log: log, commands: map[string]Command{}}
	s.Register(Command{Name: "help", Description: i18n.M("show this help")})
	return s
}

// SetSocketGroup makes the socket belong to the given group, so its members
// can use the CLI without being root. Empty keeps the default group.
func (s *Server) SetSocketGroup(group string) { s.group = group }

// Register registers a command.
func (s *Server) Register(c Command) { s.commands[c.Name] = c }

// Handle registers a text command without a description.
func (s *Server) Handle(name string, h Handler) { s.Register(Command{Name: name, Text: h}) }

// Run listens until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	// If another daemon already answers on this socket (e.g. the installed
	// service), leave it alone: on exit this copy would delete the socket and
	// the service would keep running but be unreachable for the CLI.
	if c, err := net.DialTimeout("unix", s.socketPath, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("a resguardd is already running on %s (probably the service). "+
			"There is no need to run it by hand: query it with resguardctl", s.socketPath)
	}

	// Remove a stale socket left by a previous run.
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("no permission to use %s, which belongs to the installed service. "+
				"To query it use resguardctl; to try your own daemon use make dev or another -socket", s.socketPath)
		}
		return fmt.Errorf("removing the old socket: %w", err)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("no permission to create %s: that directory belongs to the installed service. "+
			"To query it use resguardctl; to try your own daemon use make dev or another -socket", s.socketPath)
	}
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.socketPath, err)
	}
	defer ln.Close()

	if err := os.Chmod(s.socketPath, 0o660); err != nil {
		return fmt.Errorf("setting the socket permissions: %w", err)
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

	s.log.Info("listening", "socket", s.socketPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Error("accepting connection", "err", err)
			continue
		}
		go s.serve(conn)
	}
}

func chownGroup(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("looking up the socket group: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("invalid gid %q: %w", g.Gid, err)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("assigning the socket to group %s: %w", group, err)
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
				s.log.Warn("invalid request", "err", err)
			}
			return
		}

		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			s.log.Warn("sending response", "err", err)
			return
		}
	}
}

func (s *Server) dispatch(req ipc.Request) ipc.Response {
	call := Call{Args: req.Args, P: i18n.New(i18n.Lang(req.Lang))}
	if req.Command == "help" {
		return s.help(call.P)
	}
	c, ok := s.commands[req.Command]
	if !ok {
		return ipc.Response{Error: call.P.T("unknown command %q (try \"help\")", req.Command)}
	}

	var resp ipc.Response
	if c.Text != nil {
		out, err := c.Text(call)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		resp.Output = out
	}
	if c.Data != nil {
		v, err := c.Data(call)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		data, err := json.Marshal(v)
		if err != nil {
			s.log.Error("encoding response", "command", c.Name, "err", err)
			return ipc.Response{Error: call.P.T("internal error encoding the response")}
		}
		resp.Data = data
	}
	resp.OK = true
	return resp
}

// help returns the command list as text and as data (for completion), in
// the caller's language.
func (s *Server) help(p i18n.Printer) ipc.Response {
	var infos []ipc.CommandInfo
	for _, c := range s.commands {
		if c.Hidden {
			continue
		}
		usage := c.Usage
		if usage == "" {
			usage = c.Name
		}
		infos = append(infos, ipc.CommandInfo{Name: c.Name, Usage: p.T(usage), Description: p.T(c.Description)})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	for _, in := range infos {
		fmt.Fprintf(tw, "  %s\t%s\n", in.Usage, in.Description)
	}
	tw.Flush()

	data, _ := json.Marshal(infos)
	return ipc.Response{OK: true, Output: p.T("Daemon commands:") + "\n" + strings.TrimRight(b.String(), "\n"), Data: data}
}
