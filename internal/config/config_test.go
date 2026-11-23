package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

// The example file that gets installed must be valid and document exactly
// the default values.
func TestExampleFileMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("deploy/config.yaml is not valid: %v", err)
	}
	def := Default()
	def.Services.Ignore = []string{} // the example writes it as an empty list
	if !reflect.DeepEqual(cfg, def) {
		t.Errorf("deploy/config.yaml does not match the defaults:\n%s\nwant:\n%s", cfg.YAML(), def.YAML())
	}
}

func TestParsePartial(t *testing.T) {
	cfg, err := Parse([]byte("thresholds:\n  cpu: 75\nservices:\n  ignore: [snap.*]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thresholds.CPU != 75 {
		t.Errorf("cpu = %v", cfg.Thresholds.CPU)
	}
	def := Default()
	if cfg.Thresholds.Memory != def.Thresholds.Memory || cfg.Interval != def.Interval ||
		!reflect.DeepEqual(cfg.Services.Slices, def.Services.Slices) {
		t.Errorf("missing keys must take their default value: %+v", cfg)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, in := range []string{"", "# solo comentarios\n"} {
		cfg, err := Parse([]byte(in))
		if err != nil || !reflect.DeepEqual(cfg, Default()) {
			t.Errorf("Parse(%q) = %+v, %v", in, cfg, err)
		}
	}
}

func TestParseDurations(t *testing.T) {
	cfg, err := Parse([]byte("interval: 500ms\nhistory: 2h30m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 500*time.Millisecond || cfg.History != 150*time.Minute {
		t.Errorf("interval=%s history=%s", cfg.Interval, cfg.History)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, yaml string
		want       []string // fragments expected in the message
	}{
		{"misspelled key", "thresholds:\n  cpu: 80\n  memroy: 70\n", []string{"line 3", `unknown key "memroy"`}},
		{"unknown top-level key", "intervalo: 2s\n", []string{`unknown key "intervalo"`}},
		{"wrong type", "thresholds:\n  cpu: lots\n", []string{"line 2", `"lots" is not a number`}},
		{"invalid duration", "interval: fast\n", []string{"line 1", `"fast" is not a duration`}},
		{"list expected", "services:\n  ignore: snap\n", []string{"line 2", "list"}},
		{"broken syntax", "thresholds: [unclosed\n", []string{"line"}},
		{"interval too short", "interval: 10ms\n", []string{"interval"}},
		{"history shorter than the interval", "interval: 1m\nhistory: 30s\n", []string{"history"}},
		{"threshold out of range", "thresholds:\n  cpu: 150\n", []string{"thresholds.cpu", "150"}},
		{"negative threshold", "thresholds:\n  psi_io: -1\n", []string{"thresholds.psi_io"}},
		{"zero sustain", "thresholds:\n  sustain: 0\n", []string{"sustain"}},
		{"slice with a relative path", "services:\n  slices: [../../etc]\n", []string{"services.slices"}},
		{"absolute slice", "services:\n  slices: [/sys/fs/cgroup]\n", []string{"services.slices"}},
		{"no slices", "services:\n  slices: []\n", []string{"empty"}},
		{"invalid pattern", "services:\n  ignore: [\"[\"]\n", []string{"services.ignore"}},
		{"several errors at once", "interval: 1ms\nthresholds:\n  cpu: 200\n", []string{"interval", "thresholds.cpu"}},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.yaml))
		if err == nil {
			t.Errorf("%s: an error was expected", tt.name)
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not mention %q", tt.name, err, w)
			}
		}
	}
}

func TestIgnored(t *testing.T) {
	cfg := Default()
	cfg.Services.Ignore = []string{"snap.*", "packagekit.service", "user@*.service"}
	for name, want := range map[string]bool{
		"snap.lxd.daemon.service": true,
		"packagekit.service":      true,
		"user@1000.service":       true,
		"nginx.service":           false,
		"packagekit2.service":     false,
	} {
		if got := cfg.Ignored(name); got != want {
			t.Errorf("Ignored(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Thresholds.CPU = 80
	cfg.Services.Ignore = []string{"snap.*"}
	out := cfg.YAML()
	if !strings.Contains(out, "interval: 2s") || !strings.Contains(out, "history: 1h\n") || !strings.Contains(out, "cpu: 80") {
		t.Errorf("YAML:\n%s", out)
	}
	back, err := Parse([]byte(out))
	if err != nil || !reflect.DeepEqual(back, cfg) {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}

func writeConfig(t *testing.T, content string, fileMode, dirMode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "resguard")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicit chmod: WriteFile and Mkdir apply the umask.
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeConfig(t, "thresholds:\n  cpu: 70\n", 0o644, 0o755)
	cfg, found, err := Load(path, false)
	if err != nil || !found || cfg.Thresholds.CPU != 70 {
		t.Fatalf("Load = %+v, %v, %v", cfg, found, err)
	}

	missing := filepath.Join(t.TempDir(), "no-existe.yaml")
	cfg, found, err = Load(missing, true)
	if err != nil || found || !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("optional missing file: %+v, %v, %v", cfg, found, err)
	}
	if _, _, err := Load(missing, false); err == nil {
		t.Error("an explicitly requested missing file must be an error")
	}

	bad := writeConfig(t, "thresholds:\n  cpu: 500\n", 0o644, 0o755)
	if _, _, err := Load(bad, false); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("the error must name the file: %v", err)
	}
}

func TestLoadRejectsInsecurePermissions(t *testing.T) {
	tests := []struct {
		name             string
		fileMode, dirMod os.FileMode
	}{
		{"world-writable file", 0o666, 0o755},
		{"world-writable directory", 0o644, 0o777},
	}
	for _, tt := range tests {
		path := writeConfig(t, "interval: 2s\n", tt.fileMode, tt.dirMod)
		_, _, err := Load(path, false)
		if err == nil || !strings.Contains(err.Error(), "other users") {
			t.Errorf("%s: a permission rejection was expected, error = %v", tt.name, err)
		}
	}
}

// The owner's personal group (usual on Ubuntu, umask 002) is acceptable;
// another group with write access is not.
func TestLoadGroupWritable(t *testing.T) {
	path := writeConfig(t, "interval: 2s\n", 0o664, 0o775)
	if _, _, err := Load(path, false); err != nil {
		t.Fatalf("writable only by the own group should be accepted: %v", err)
	}

	groups, err := os.Getgroups()
	if err != nil {
		t.Skip(err)
	}
	own := os.Getgid()
	for _, g := range groups {
		if g == own {
			continue
		}
		if err := os.Chown(path, -1, g); err != nil {
			continue
		}
		if _, _, err := Load(path, false); err == nil || !strings.Contains(err.Error(), "other users") {
			t.Errorf("writable by shared group %d should be rejected: %v", g, err)
		}
		return
	}
	t.Skip("the user belongs to no other group to test with")
}

func TestErrorsDoNotMentionGoTypes(t *testing.T) {
	for _, in := range []string{"thresholds:\n  memroy: 1\n", "thresholds:\n  cpu: x\n", "interval: x\n", "services:\n  ignore: x\n"} {
		_, err := Parse([]byte(in))
		if err == nil {
			t.Fatalf("Parse(%q) should fail", in)
		}
		for _, internal := range []string{"events.", "float64", "time.Duration", "[]string", "unmarshal", "yaml:"} {
			if strings.Contains(err.Error(), internal) {
				t.Errorf("the error for %q mentions %q: %v", in, internal, err)
			}
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: "1h", 90 * time.Minute: "1h30m", 2 * time.Second: "2s",
		500 * time.Millisecond: "500ms", 5 * time.Minute: "5m", time.Hour + 5*time.Second: "1h0m5s",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
