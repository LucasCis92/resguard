// Package i18n translates resguard's user-facing text.
//
// Source strings are written in English and double as catalog keys
// (gettext style): p.T("CPU usage") returns "uso de CPU" for Spanish. A test
// scans the code for T and M calls and fails if a string has no translation,
// or if a translation has different format verbs than its source.
package i18n

import (
	"fmt"
	"os"
	"strings"
)

// Lang is a supported language.
type Lang string

const (
	English Lang = "en"
	Spanish Lang = "es"
)

// catalogs maps each non-English language to its translations.
var catalogs = map[Lang]map[string]string{
	Spanish: spanish,
}

// Parse turns a language or locale name ("es", "es_AR.UTF-8", "en") into a
// supported language. Anything that is not Spanish falls back to English.
func Parse(s string) Lang {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "es" || strings.HasPrefix(s, "es_") || strings.HasPrefix(s, "es-") || strings.HasPrefix(s, "es.") {
		return Spanish
	}
	return English
}

// Detect returns the user's language. RESGUARD_LANG wins; otherwise the
// usual locale variables are used, in the order gettext honors them.
func Detect() Lang {
	if v := os.Getenv("RESGUARD_LANG"); v != "" {
		return Parse(v)
	}
	locale := ""
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			locale = v
			break
		}
	}
	if locale == "C" || locale == "POSIX" || strings.HasPrefix(locale, "C.") {
		return English
	}
	// LANGUAGE is a priority list ("es:en") that refines the locale.
	if v := os.Getenv("LANGUAGE"); v != "" && locale != "" {
		return Parse(strings.Split(v, ":")[0])
	}
	return Parse(locale)
}

// Printer translates into one language.
type Printer struct {
	lang Lang
}

// New returns a printer for lang.
func New(lang Lang) Printer { return Printer{lang: Parse(string(lang))} }

// Lang returns the printer's language.
func (p Printer) Lang() Lang {
	if p.lang == "" {
		return English
	}
	return p.lang
}

// T translates format and fills in its arguments like fmt.Sprintf. Without a
// translation, the English source is used.
func (p Printer) T(format string, a ...any) string {
	if tr, ok := catalogs[p.lang][format]; ok {
		format = tr
	}
	if len(a) == 0 {
		return format
	}
	return fmt.Sprintf(format, a...)
}

// M marks a string for translation without translating it, for text that is
// stored now and translated later with T (such as command descriptions).
func M(s string) string { return s }
