// Package config loads and validates resguardd's configuration file (by
// default /etc/resguard/config.yaml).
//
// Every key is optional: whatever the file omits takes its default value, and
// without a file the daemon runs with the defaults.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/LucasCis92/resguard/internal/events"
)

// DefaultPath is the file's default location.
const DefaultPath = "/etc/resguard/config.yaml"

// Config is the daemon's configuration.
type Config struct {
	Interval   time.Duration     `yaml:"interval"`
	History    time.Duration     `yaml:"history"`
	Thresholds events.Thresholds `yaml:"thresholds"`
	Services   Services          `yaml:"services"`
}

// Services selects which units are monitored.
type Services struct {
	// Cgroup slices to scan for services, applications and containers.
	Slices []string `yaml:"slices"`
	// Units that never raise incidents. Wildcards are allowed: "snap.*".
	Ignore []string `yaml:"ignore"`
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		Interval:   2 * time.Second,
		History:    time.Hour,
		Thresholds: events.DefaultThresholds,
		Services:   Services{Slices: []string{"system.slice", "user.slice"}},
	}
}

// Load reads the file at path on top of the defaults. If the file does not
// exist and missingOK is true, it returns the defaults and found=false.
func Load(path string, missingOK bool) (cfg Config, found bool, err error) {
	cfg = Default()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) && missingOK {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, err
	}
	defer f.Close()

	if err := checkPermissions(f, path); err != nil {
		return cfg, true, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return cfg, true, err
	}
	cfg, err = Parse(data)
	if err != nil {
		return cfg, true, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, true, nil
}

// Parse decodes YAML content on top of the defaults and validates it.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a misspelled key is an error, not silently ignored
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("invalid format: %w", cleanYAMLError(err))
	}
	return cfg, cfg.Validate()
}

// Validate checks that the values make sense.
func (c Config) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Interval < 100*time.Millisecond || c.Interval > time.Hour {
		bad("interval must be between 100ms and 1h (it is %s)", c.Interval)
	}
	if c.History < c.Interval || c.History > 7*24*time.Hour {
		bad("history must be between interval and 168h (it is %s)", c.History)
	} else if c.Interval > 0 && c.History/c.Interval > 1_000_000 {
		bad("history/interval gives more than 1,000,000 samples; increase interval or reduce history")
	}

	t := c.Thresholds
	for _, th := range []struct {
		name string
		v    float64
	}{{"cpu", t.CPU}, {"memory", t.Memory}, {"psi_cpu", t.PSICPU}, {"psi_memory", t.PSIMemory}, {"psi_io", t.PSIIO}} {
		if th.v < 0 || th.v > 100 {
			bad("thresholds.%s must be between 0 (disabled) and 100 (it is %g)", th.name, th.v)
		}
	}
	if t.Sustain < 1 || t.Sustain > 1000 {
		bad("thresholds.sustain must be between 1 and 1000 (it is %d)", t.Sustain)
	}

	if len(c.Services.Slices) == 0 {
		bad("services.slices cannot be empty")
	}
	for _, s := range c.Services.Slices {
		if s == "" || filepath.IsAbs(s) || strings.Contains(s, "..") || !strings.HasSuffix(s, ".slice") && !strings.HasSuffix(s, ".service") {
			bad("services.slices: %q is not a valid cgroup path (e.g. system.slice)", s)
		}
	}
	for _, p := range c.Services.Ignore {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			bad("services.ignore: invalid pattern %q", p)
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Ignored reports whether a unit is excluded from incidents.
func (c Config) Ignored(service string) bool {
	for _, p := range c.Services.Ignore {
		if ok, _ := path.Match(p, service); ok {
			return true
		}
	}
	return false
}

// YAML returns the configuration as YAML, with readable durations ("1h"
// instead of "1h0m0s").
func (c Config) YAML() string {
	out := struct {
		Interval   string            `yaml:"interval"`
		History    string            `yaml:"history"`
		Thresholds events.Thresholds `yaml:"thresholds"`
		Services   Services          `yaml:"services"`
	}{formatDuration(c.Interval), formatDuration(c.History), c.Thresholds, c.Services}

	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	enc.Encode(out)
	enc.Close()
	return strings.TrimRight(b.String(), "\n")
}

func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// checkPermissions rejects the file if anyone other than its owner (root or
// the daemon's user) can modify it: they could, for example, disable every
// alert. sshd performs the same check.
func checkPermissions(f *os.File, path string) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := checkOwnerAndMode(fi, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	return checkOwnerAndMode(di, dir)
}

func checkOwnerAndMode(fi os.FileInfo, name string) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := uint32(os.Getuid()); st.Uid != 0 && st.Uid != uid {
		return fmt.Errorf("%s belongs to another user (uid %d); it must belong to root", name, st.Uid)
	}
	perm := fi.Mode().Perm()
	if fi.IsDir() && fi.Mode()&os.ModeSticky != 0 {
		return nil // like /tmp: others can create files but not modify someone else's
	}
	insecure := perm&0o002 != 0
	// Group write access is only acceptable for the owner's own group
	// (root:root, or the personal group Debian and Ubuntu create per user).
	if perm&0o020 != 0 && st.Gid != primaryGID(st.Uid) {
		insecure = true
	}
	if insecure {
		return fmt.Errorf("%s can be modified by other users (mode %04o); fix it with: sudo chmod go-w %s", name, perm, name)
	}
	return nil
}

// primaryGID returns a user's primary group, or an impossible value if it
// cannot be found out (so the file is treated as insecure).
func primaryGID(uid uint32) uint32 {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return ^uint32(0)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return ^uint32(0)
	}
	return uint32(gid)
}

var yamlErrorRewrites = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`yaml: `), ""},
	{regexp.MustCompile(`unmarshal errors:\n\s*`), ""},
	{regexp.MustCompile(`field (\S+) not found in type \S+`), `unknown key "$1" (is it spelled correctly?)`},
	{regexp.MustCompile("cannot unmarshal !!\\w+ `([^`]*)` into (float64|int)"), `"$1" is not a number`},
	{regexp.MustCompile("cannot unmarshal !!\\w+ `([^`]*)` into time.Duration"), `"$1" is not a duration (examples: 500ms, 2s, 1m, 1h)`},
	{regexp.MustCompile("cannot unmarshal !!\\w+ `([^`]*)` into \\[\\]string"), `"$1" should be a list, for example [a, b]`},
}

// cleanYAMLError rewrites the YAML library's errors into messages that don't
// mention Go's internal types.
func cleanYAMLError(err error) error {
	msg := err.Error()
	for _, r := range yamlErrorRewrites {
		msg = r.re.ReplaceAllString(msg, r.repl)
	}
	return errors.New(msg)
}
