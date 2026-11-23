package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
)

// localCommands are the commands the CLI handles itself, without the daemon.
// Usage and descriptions are English and are translated when shown.
var localCommands = []ipc.CommandInfo{
	{Name: "watch", Usage: i18n.M("watch [seconds]"),
		Description: i18n.M("live view; ↑↓ select, Enter processes, a/s/d/t filter, c/m sort, q quit")},
	{Name: "logs", Usage: i18n.M("logs [n] [warnings] [follow]"),
		Description: i18n.M("service logs (journal); follow = live, Ctrl+C to go back")},
	{Name: "clear", Description: i18n.M("clear the screen")},
	{Name: "exit", Description: i18n.M("quit (also Ctrl+D or Ctrl+C)")},
}

// session keeps the connection to the daemon and reopens it if it drops, for
// example because the service restarted.
type session struct {
	socket   string
	p        i18n.Printer
	client   *ipc.Client
	commands []ipc.CommandInfo // daemon commands, for help and completion

	interactive bool // in interactive mode, help includes clear, exit and the shortcuts
}

func newSession(socket string, p i18n.Printer) (*session, error) {
	s := &session{socket: socket, p: p}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) connect() error {
	c, err := ipc.Dial(s.socket)
	if err != nil {
		return err
	}
	c.Lang = string(s.p.Lang())
	s.client = c
	return nil
}

func (s *session) close() {
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

// connectError explains a failure to reach the daemon.
func (s *session) connectError(err error) string {
	cause := err
	if u := errors.Unwrap(err); u != nil {
		cause = u
	}
	msg := s.p.T("cannot connect to the daemon at %s: %v", s.socket, cause)
	if errors.Is(err, fs.ErrPermission) {
		msg += "\n" + s.p.T("Hint: use sudo, or add your user to the resguard group (sudo usermod -aG resguard $USER) and log in again.")
	} else if errors.Is(err, fs.ErrNotExist) {
		msg += "\n" + s.p.T("Hint: is the service running? Check with: systemctl status resguard")
	}
	return msg
}

// call sends a command. If the connection had dropped, it reconnects and
// retries once. Every current command is read-only, so repeating is safe.
func (s *session) call(command string, args ...string) (ipc.Response, error) {
	if s.client == nil {
		if err := s.connect(); err != nil {
			return ipc.Response{}, err
		}
	}
	resp, err := s.client.Call(command, args...)
	if err == nil {
		return resp, nil
	}
	s.close()
	if err := s.connect(); err != nil {
		return ipc.Response{}, err
	}
	return s.client.Call(command, args...)
}

// loadCommands asks the daemon for its command list. If it fails, completion
// is limited to the local commands.
func (s *session) loadCommands() {
	resp, err := s.call("help")
	if err != nil || !resp.OK {
		return
	}
	var infos []ipc.CommandInfo
	if resp.Decode(&infos) == nil {
		s.commands = infos
	}
}

// commandNames returns the daemon's commands and the local ones.
func (s *session) commandNames() []string {
	var names []string
	for _, c := range s.commands {
		names = append(names, c.Name)
	}
	for _, c := range localCommands {
		names = append(names, c.Name)
	}
	return names
}

// run executes a daemon command and writes the result to out. It returns
// false if the command failed.
func (s *session) run(fields []string, out io.Writer) bool {
	resp, err := s.call(fields[0], fields[1:]...)
	if err != nil {
		fmt.Fprintln(out, s.p.T("error:"), s.connectError(err))
		return false
	}
	if !resp.OK {
		fmt.Fprintln(out, s.p.T("error:"), resp.Error)
		return false
	}
	if fields[0] == "help" {
		resp.Output += "\n\n" + localHelp(s.interactive, s.p)
	}
	if resp.Output != "" {
		fmt.Fprintln(out, resp.Output)
	}
	return true
}

func localHelp(interactive bool, p i18n.Printer) string {
	var b strings.Builder
	b.WriteString(p.T("CLI commands:") + "\n")
	for _, c := range localCommands {
		if !interactive && (c.Name == "clear" || c.Name == "exit") {
			continue
		}
		usage := c.Name
		if c.Usage != "" {
			usage = p.T(c.Usage)
		}
		fmt.Fprintf(&b, "  %-26s %s\n", usage, p.T(c.Description))
	}
	if interactive {
		b.WriteString("\n" + p.T("Shortcuts: up/down arrows for history, Tab to complete."))
	}
	b.WriteString("\n" + p.T("Language: set RESGUARD_LANG=en or RESGUARD_LANG=es to override the system locale."))
	return strings.TrimRight(b.String(), "\n")
}
