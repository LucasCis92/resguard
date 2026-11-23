package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// history keeps the commands typed in interactive mode so they can be
// recalled with the arrow keys, also across sessions. It implements
// term.History.
//
// The file is private to the user (0600, in a 0700 directory) and, as in
// bash, lines starting with a space are not saved.
type history struct {
	entries []string // oldest first
	max     int
	path    string // empty: memory only
}

// historyPath returns $XDG_STATE_HOME/resguard/history or
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

// loadHistory loads the history from path. If it cannot be read or created,
// the history still works, in memory only.
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

// Add adds an entry. term.Terminal calls it for every line read.
func (h *history) Add(entry string) {
	if strings.TrimSpace(entry) == "" || strings.HasPrefix(entry, " ") {
		return
	}
	entry = strings.TrimRight(entry, " \t") // completion leaves a trailing space
	if n := len(h.entries); n > 0 && h.entries[n-1] == entry {
		return
	}
	h.entries = append(h.entries, entry)
	if len(h.entries) > h.max {
		h.entries = h.entries[1:]
	}
	h.append(entry)
}

// Len returns the number of entries.
func (h *history) Len() int { return len(h.entries) }

// At returns an entry; 0 is the most recent.
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
