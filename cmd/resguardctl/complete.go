package main

import (
	"sort"
	"strings"
)

// completer completes command names and their first argument with Tab.
type completer struct {
	commands func() []string     // available commands (daemon and local)
	args     map[string][]string // possible values of each command's first argument
}

// complete computes the result of pressing Tab with the cursor at pos. With
// one option it completes it; with several it completes their common prefix
// and, if that adds nothing, returns the options so they can be shown.
func (c *completer) complete(line string, pos int) (newLine string, newPos int, options []string) {
	before, after := line[:pos], line[pos:]
	start := strings.LastIndexAny(before, " \t") + 1
	word := before[start:]
	prev := strings.Fields(before[:start])

	var candidates []string
	switch len(prev) {
	case 0:
		candidates = c.commands()
	case 1:
		if prev[0] == "help" {
			candidates = c.commands()
		} else {
			candidates = c.args[prev[0]]
		}
	}

	var matches []string
	for _, cand := range candidates {
		if strings.HasPrefix(cand, word) {
			matches = append(matches, cand)
		}
	}
	sort.Strings(matches)
	matches = dedup(matches)

	switch len(matches) {
	case 0:
		return line, pos, nil
	case 1:
		completed := matches[0]
		if !strings.HasPrefix(after, " ") {
			completed += " "
		}
		return before[:start] + completed + after, start + len(completed), nil
	}

	prefix := commonPrefix(matches)
	if len(prefix) > len(word) {
		return before[:start] + prefix + after, start + len(prefix), nil
	}
	return line, pos, matches
}

func commonPrefix(words []string) string {
	p := words[0]
	for _, w := range words[1:] {
		for !strings.HasPrefix(w, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

func dedup(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
