package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// history guarda las órdenes escritas en el modo interactivo para recuperarlas
// con las flechas, también entre sesiones. Implementa term.History.
//
// El fichero es privado del usuario (0600, en un directorio 0700) y, como en
// bash, las líneas que empiezan por espacio no se guardan.
type history struct {
	entries []string // de la más antigua a la más reciente
	max     int
	path    string // vacío: solo en memoria
}

// historyPath devuelve $XDG_STATE_HOME/resguard/history o
// ~/.local/state/resguard/history.
func historyPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "resguard", "history")
}

// loadHistory carga el historial de path. Si no se puede leer o crear, el
// historial funciona igual pero solo en memoria.
func loadHistory(path string, max int) *history {
	h := &history{max: max, path: path}
	if path == "" {
		return h
	}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				h.entries = append(h.entries, line)
			}
		}
		f.Close()
	}
	if len(h.entries) > max {
		h.entries = h.entries[len(h.entries)-max:]
		h.rewrite()
	}
	return h
}

// Add añade una entrada. La llama term.Terminal con cada línea leída.
func (h *history) Add(entry string) {
	if strings.TrimSpace(entry) == "" || strings.HasPrefix(entry, " ") {
		return
	}
	entry = strings.TrimRight(entry, " \t") // el autocompletado deja un espacio final
	if n := len(h.entries); n > 0 && h.entries[n-1] == entry {
		return
	}
	h.entries = append(h.entries, entry)
	if len(h.entries) > h.max {
		h.entries = h.entries[1:]
	}
	h.append(entry)
}

// Len devuelve el número de entradas.
func (h *history) Len() int { return len(h.entries) }

// At devuelve una entrada; 0 es la más reciente.
func (h *history) At(idx int) string { return h.entries[len(h.entries)-1-idx] }

func (h *history) append(entry string) {
	if h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(entry + "\n")
}

func (h *history) rewrite() {
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(h.entries, "\n")+"\n"), 0o600); err != nil {
		return
	}
	os.Rename(tmp, h.path)
}
