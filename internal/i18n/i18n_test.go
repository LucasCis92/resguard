package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sourceStrings returns every string literal passed to a T method or to M in
// the non-test Go files of the repository.
func sourceStrings(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{} // string → first position
	fset := token.NewFileSet()
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist" || d.Name() == "build") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if name != "T" && name != "M" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil {
				if _, seen := found[s]; !seen {
					found[s] = fset.Position(lit.Pos()).String()
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestEveryStringIsTranslated(t *testing.T) {
	src := sourceStrings(t)
	if len(src) < 50 {
		t.Fatalf("only %d translatable strings found; is the scanner broken?", len(src))
	}
	var missing []string
	for s, pos := range src {
		if _, ok := spanish[s]; !ok {
			missing = append(missing, pos+": "+strconv.Quote(s))
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("missing Spanish translation: %s", m)
	}
}

func TestNoUnusedTranslations(t *testing.T) {
	src := sourceStrings(t)
	for k := range spanish {
		if _, ok := src[k]; !ok {
			t.Errorf("translation not used by the code (remove it): %q", k)
		}
	}
}

var verb = regexp.MustCompile(`%[-+# 0]*[0-9*]*(?:\.[0-9*]+)?[a-zA-Z%]`)

func TestTranslationsKeepFormatVerbs(t *testing.T) {
	for src, tr := range spanish {
		if a, b := verb.FindAllString(src, -1), verb.FindAllString(tr, -1); strings.Join(a, " ") != strings.Join(b, " ") {
			t.Errorf("format verbs differ:\n  en %q %v\n  es %q %v", src, a, tr, b)
		}
		if strings.TrimSpace(tr) == "" {
			t.Errorf("empty translation for %q", src)
		}
	}
}

func TestParseAndDetect(t *testing.T) {
	for in, want := range map[string]Lang{
		"es": Spanish, "es_AR.UTF-8": Spanish, "ES-mx": Spanish, "en_US.UTF-8": English,
		"en": English, "fr_FR.UTF-8": English, "": English, "C": English, "estonian": English,
	} {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %s, want %s", in, got, want)
		}
	}

	tests := []struct {
		env  map[string]string
		want Lang
	}{
		{map[string]string{"LANG": "es_AR.UTF-8"}, Spanish},
		{map[string]string{"LANG": "en_US.UTF-8", "LANGUAGE": "en"}, English},
		{map[string]string{"LANG": "en_US.UTF-8", "LANGUAGE": "es:en"}, Spanish},
		{map[string]string{"LANG": "es_ES.UTF-8", "LC_MESSAGES": "en_GB.UTF-8"}, English},
		{map[string]string{"LANG": "es_ES.UTF-8", "LC_ALL": "C"}, English},
		{map[string]string{"LANG": "en_US.UTF-8", "RESGUARD_LANG": "es"}, Spanish},
		{map[string]string{"LANG": "es_ES.UTF-8", "RESGUARD_LANG": "en"}, English},
		{map[string]string{}, English},
	}
	for _, tt := range tests {
		for _, k := range []string{"RESGUARD_LANG", "LC_ALL", "LC_MESSAGES", "LANG", "LANGUAGE"} {
			t.Setenv(k, tt.env[k])
		}
		if got := Detect(); got != tt.want {
			t.Errorf("Detect() with %v = %s, want %s", tt.env, got, tt.want)
		}
	}
}

func TestPrinter(t *testing.T) {
	es, en := New(Spanish), New(English)
	if got := es.T("CPU usage"); got != spanish["CPU usage"] {
		t.Errorf("es.T = %q", got)
	}
	if got := en.T("CPU usage"); got != "CPU usage" {
		t.Errorf("en.T = %q", got)
	}
	if got := es.T("no translation for this %d", 3); got != "no translation for this 3" {
		t.Errorf("fallback = %q", got)
	}
	if (Printer{}).Lang() != English {
		t.Error("the zero Printer must be English")
	}
}
