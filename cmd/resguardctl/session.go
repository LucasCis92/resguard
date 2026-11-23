package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/LucasCis92/resguard/internal/ipc"
)

// localCommands son las órdenes que resuelve la propia CLI, sin el demonio.
var localCommands = []ipc.CommandInfo{
	{Name: "watch", Usage: "watch [segundos]", Description: "vista en vivo; ↑↓ elegir, Enter ver procesos, a/s/d/t filtrar, c/m ordenar, q salir"},
	{Name: "logs", Usage: "logs [n] [avisos] [seguir]", Description: "logs del servicio (journal); seguir = en vivo, Ctrl+C para volver"},
	{Name: "clear", Description: "limpia la pantalla"},
	{Name: "exit", Description: "sale (también Ctrl+D o Ctrl+C)"},
}

// session mantiene la conexión con el demonio y la reabre si se corta,
// por ejemplo porque el servicio se reinició.
type session struct {
	socket   string
	client   *ipc.Client
	commands []ipc.CommandInfo // órdenes del demonio, para ayuda y autocompletado

	interactive bool // en el modo interactivo la ayuda incluye clear, exit y los atajos
}

func newSession(socket string) (*session, error) {
	s := &session{socket: socket}
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
	s.client = c
	return nil
}

func (s *session) close() {
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

// call envía una orden. Si la conexión se había cortado, reconecta y la
// reintenta una vez. Todas las órdenes actuales son de solo lectura, así que
// repetirlas es seguro.
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

// loadCommands pide al demonio la lista de órdenes. Si falla, el
// autocompletado se limita a las órdenes locales.
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

// commandNames devuelve las órdenes del demonio y las locales.
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

// run ejecuta una orden del demonio y escribe el resultado en out.
// Devuelve false si falló.
func (s *session) run(fields []string, out io.Writer) bool {
	resp, err := s.call(fields[0], fields[1:]...)
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return false
	}
	if !resp.OK {
		fmt.Fprintln(out, "error:", resp.Error)
		return false
	}
	if fields[0] == "help" {
		resp.Output += "\n\n" + localHelp(s.interactive)
	}
	if resp.Output != "" {
		fmt.Fprintln(out, resp.Output)
	}
	return true
}

func localHelp(interactive bool) string {
	var b strings.Builder
	b.WriteString("Órdenes de la CLI:\n")
	for _, c := range localCommands {
		if !interactive && (c.Name == "clear" || c.Name == "exit") {
			continue
		}
		usage := c.Usage
		if usage == "" {
			usage = c.Name
		}
		fmt.Fprintf(&b, "  %-22s %s\n", usage, c.Description)
	}
	if interactive {
		b.WriteString("\nAtajos: flechas arriba/abajo para el historial, Tab para autocompletar.")
	}
	return strings.TrimRight(b.String(), "\n")
}
