package metrics

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupRoot is the path of the cgroup v2 unified hierarchy, relative to Root.
const CgroupRoot = "sys/fs/cgroup"

// Kinds of monitored units. They are identifiers, not display text.
const (
	KindService   = "service"   // systemd service (nginx.service)
	KindApp       = "app"       // desktop session application (Chrome, VS Code)
	KindContainer = "container" // Docker or Podman container
	KindSession   = "session"   // login session (ssh, console)
	KindOther     = "other"     // other scopes (e.g. systemd-run --scope)
)

// Service holds the cgroup counters of a systemd unit: a service, a desktop
// application, a container or a session.
type Service struct {
	Name         string // unit name, e.g. "nginx.service" or "app-com.google.Chrome-5016.scope"
	Display      string // readable, language-neutral name, e.g. "Chrome" or "laravel_app"
	Kind         string // one of the Kind* constants
	Cgroup       string // cgroup path relative to the cgroup root
	MemCurrent   uint64 // bytes in use (memory.current)
	MemMax       uint64 // limit in bytes (memory.max); 0 = no limit
	CPUUsageUsec uint64 // cumulative CPU time (cpu.stat usage_usec)
	OOM          uint64 // times the limit was hit and the OOM killer ran (memory.events oom)
	OOMKills     uint64 // processes killed by the OOM killer (memory.events oom_kill)
}

// ReadServices walks the given slices (e.g. "system.slice" or "user.slice")
// and returns the counters of every unit it finds: services (including those
// in nested slices such as system-getty.slice), application and container
// scopes, and the applications of user sessions (inside user@UID.service).
func (r *Reader) ReadServices(slices []string) ([]Service, error) {
	base := r.path(CgroupRoot)
	var out []Service
	for _, slice := range slices {
		dir := filepath.Join(base, slice)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A cgroup can vanish while we walk it.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.IsDir() || path == dir {
				return nil
			}
			name := d.Name()
			rel, _ := filepath.Rel(base, path)
			switch {
			case strings.HasSuffix(name, ".slice"):
				return nil // descend into nested slices
			case strings.HasPrefix(name, "user@") && strings.HasSuffix(name, ".service"):
				return nil // a user session manager: its applications are inside
			case strings.HasSuffix(name, ".service"), strings.HasSuffix(name, ".scope"):
				if svc, ok := readService(path, name, rel); ok {
					svc.Kind = kindOf(name, rel)
					svc.Display = r.displayName(name, rel, svc.Kind)
					out = append(out, svc)
				}
				return fs.SkipDir // a unit's sub-cgroups are not separate units
			default:
				return fs.SkipDir
			}
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return out, err
		}
	}
	unifyAppNames(out)
	return out, nil
}

// unifyAppNames gives the same name to the units of a single application
// launch. GNOME sometimes creates two (app-com.google.Chrome-5016.scope and
// app-gnome-google\x2dchrome-5016.scope) that share the final suffix. The name
// derived from the application ID (Chrome) is preferred.
func unifyAppNames(units []Service) {
	best := map[string]string{}
	for _, u := range units {
		key := launchID(u)
		if key == "" {
			continue
		}
		if cur, ok := best[key]; !ok || betterName(u.Display, cur) {
			best[key] = u.Display
		}
	}
	for i, u := range units {
		if key := launchID(u); key != "" {
			units[i].Display = best[key]
		}
	}
}

// launchID returns the numeric suffix of an application unit (the "5016" of
// app-com.google.Chrome-5016.scope), or "".
func launchID(u Service) string {
	if u.Kind != KindApp || !strings.HasPrefix(u.Name, "app-") {
		return ""
	}
	base := strings.TrimSuffix(strings.TrimSuffix(u.Name, ".scope"), ".service")
	i := strings.LastIndex(base, "-")
	if i < 0 || !isDigits(base[i+1:]) {
		return ""
	}
	return base[i+1:]
}

func betterName(a, b string) bool {
	hasUpper := func(s string) bool { return strings.ToLower(s) != s }
	if hasUpper(a) != hasUpper(b) {
		return hasUpper(a)
	}
	return len(a) < len(b)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func kindOf(name, rel string) string {
	switch {
	case strings.HasPrefix(name, "docker-") && strings.HasSuffix(name, ".scope"),
		strings.HasPrefix(name, "libpod-") && strings.HasSuffix(name, ".scope"):
		return KindContainer
	case strings.Contains(rel, "/app.slice/"):
		return KindApp
	case strings.HasPrefix(name, "session-") && strings.HasSuffix(name, ".scope"):
		return KindSession
	case strings.HasSuffix(name, ".service"):
		return KindService
	default:
		return KindOther
	}
}

// displayName computes a readable, language-neutral name for a unit.
func (r *Reader) displayName(name, rel, kind string) string {
	switch kind {
	case KindContainer:
		id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(name, "docker-"), "libpod-"), ".scope")
		if n := r.containerName(id); n != "" {
			return n
		}
		if len(id) > 12 {
			id = id[:12]
		}
		return id
	case KindSession:
		return strings.TrimSuffix(name, ".scope")
	case KindApp:
		// A terminal tab (vte-spawn-….scope) hangs from
		// app-org.gnome.Terminal.slice: use the application's name.
		if !strings.HasPrefix(name, "app-") {
			parts := strings.Split(rel, "/")
			for i := len(parts) - 2; i >= 0; i-- {
				if strings.HasPrefix(parts[i], "app-") {
					return appName(parts[i])
				}
			}
			return name
		}
		return appName(name)
	default:
		return name
	}
}

// appName turns the unit name of a desktop application, in the format
// app-[<launcher>-]<id>[-<random>].scope, into a short name:
// app-com.google.Chrome-5016.scope → Chrome, app-gnome-code-6255.scope → code.
func appName(unit string) string {
	s := unit
	for _, suf := range []string{".scope", ".service", ".slice"} {
		s = strings.TrimSuffix(s, suf)
	}
	s = strings.TrimPrefix(s, "app-")
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	for _, launcher := range []string{"gnome-", "kde-", "flatpak-", "snap-", "dbus-", "systemd-"} {
		if strings.HasPrefix(s, launcher) && len(s) > len(launcher) {
			s = strings.TrimPrefix(s, launcher)
			break
		}
	}
	// Random suffix added by the launcher (decimal or hexadecimal).
	if i := strings.LastIndex(s, "-"); i > 0 && isHex(s[i+1:]) {
		s = s[:i]
	}
	s = unescapeUnit(s)
	// Reverse-DNS identifiers: com.google.Chrome → Chrome.
	if parts := strings.Split(s, "."); len(parts) >= 3 {
		s = parts[len(parts)-1]
	}
	if s == "" {
		return unit
	}
	return s
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// unescapeUnit undoes systemd escaping: "google\x2dchrome" → "google-chrome".
func unescapeUnit(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// containerName looks up a Docker container's name in its configuration
// (requires root; returns "" if it cannot be read). Results are cached.
func (r *Reader) containerName(id string) string {
	if n, ok := r.containerNames[id]; ok {
		return n
	}
	if r.containerNames == nil {
		r.containerNames = map[string]string{}
	}
	if len(r.containerNames) > 1000 { // containers come and go: don't grow without bound
		r.containerNames = map[string]string{}
	}
	name := ""
	f, err := os.Open(r.path(filepath.Join("var/lib/docker/containers", filepath.Base(id), "config.v2.json")))
	if err == nil {
		var cfg struct{ Name string }
		if json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&cfg) == nil {
			name = strings.TrimPrefix(cfg.Name, "/")
		}
		f.Close()
	}
	r.containerNames[id] = name
	return name
}

func readService(path, name, rel string) (Service, bool) {
	svc := Service{Name: name, Cgroup: rel}

	cpu, err := readKeyValues(filepath.Join(path, "cpu.stat"))
	if err != nil {
		return svc, false
	}
	svc.CPUUsageUsec = cpu["usage_usec"]

	// The memory controller may not be enabled for this cgroup.
	if ev, err := readKeyValues(filepath.Join(path, "memory.events")); err == nil {
		svc.OOM = ev["oom"]
		svc.OOMKills = ev["oom_kill"]
	}
	svc.MemCurrent = readUint(filepath.Join(path, "memory.current"))
	svc.MemMax = readUint(filepath.Join(path, "memory.max")) // "max" reads as 0
	return svc, true
}

func readUint(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
