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

// CgroupRoot es la ruta de la jerarquía unificada de cgroups v2, relativa a Root.
const CgroupRoot = "sys/fs/cgroup"

// Tipos de unidad que se vigilan.
const (
	KindService   = "servicio"   // servicio de systemd (nginx.service)
	KindApp       = "app"        // aplicación de una sesión de escritorio (Chrome, VS Code)
	KindContainer = "contenedor" // contenedor Docker o Podman
	KindSession   = "sesión"     // sesión de inicio (ssh, consola)
	KindOther     = "otro"       // otros scopes (p. ej. systemd-run --scope)
)

// Service son los contadores del cgroup de una unidad de systemd: un servicio,
// una aplicación de escritorio, un contenedor o una sesión.
type Service struct {
	Name         string // nombre de la unidad, p. ej. "nginx.service" o "app-com.google.Chrome-5016.scope"
	Display      string // nombre legible, p. ej. "Chrome" o "laravel_app"
	Kind         string // uno de los Kind*
	Cgroup       string // ruta del cgroup relativa a la raíz de cgroups
	MemCurrent   uint64 // bytes en uso (memory.current)
	MemMax       uint64 // límite en bytes (memory.max); 0 = sin límite
	CPUUsageUsec uint64 // tiempo de CPU acumulado (cpu.stat usage_usec)
	OOM          uint64 // veces que se alcanzó el límite y actuó el OOM (memory.events oom)
	OOMKills     uint64 // procesos matados por el OOM killer (memory.events oom_kill)
}

// ReadServices recorre los slices indicados (p. ej. "system.slice" o
// "user.slice") y devuelve los contadores de cada unidad que encuentre:
// servicios (también los de slices anidados como system-getty.slice), scopes
// de aplicaciones y contenedores, y las aplicaciones de las sesiones de
// usuario (dentro de user@UID.service).
func (r *Reader) ReadServices(slices []string) ([]Service, error) {
	base := r.path(CgroupRoot)
	var out []Service
	for _, slice := range slices {
		dir := filepath.Join(base, slice)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// Un cgroup puede desaparecer mientras lo recorremos.
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
				return nil // bajar a los slices anidados
			case strings.HasPrefix(name, "user@") && strings.HasSuffix(name, ".service"):
				return nil // gestor de una sesión de usuario: sus aplicaciones están dentro
			case strings.HasSuffix(name, ".service"), strings.HasSuffix(name, ".scope"):
				if svc, ok := readService(path, name, rel); ok {
					svc.Kind = kindOf(name, rel)
					svc.Display = r.displayName(name, rel, svc.Kind)
					out = append(out, svc)
				}
				return fs.SkipDir // los subcgroups de una unidad no son unidades aparte
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

// unifyAppNames da el mismo nombre a las unidades de una misma apertura de
// una aplicación. GNOME crea a veces dos (app-com.google.Chrome-5016.scope y
// app-gnome-google\x2dchrome-5016.scope): comparten el sufijo final. Se
// prefiere el nombre derivado del identificador de la aplicación (Chrome).
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

// launchID devuelve el sufijo numérico de la unidad de una aplicación
// (el "5016" de app-com.google.Chrome-5016.scope), o "".
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

// displayName calcula un nombre legible para una unidad.
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
		return "contenedor " + id
	case KindSession:
		return "sesión " + strings.TrimSuffix(strings.TrimPrefix(name, "session-"), ".scope")
	case KindApp:
		// Una pestaña de terminal (vte-spawn-….scope) cuelga de
		// app-org.gnome.Terminal.slice: se usa el nombre de la aplicación.
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

// appName convierte el nombre de la unidad de una aplicación de escritorio,
// con el formato app-[<lanzador>-]<id>[-<aleatorio>].scope, en un nombre corto:
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
	// Sufijo aleatorio que añade el lanzador (números o hexadecimal).
	if i := strings.LastIndex(s, "-"); i > 0 && isHex(s[i+1:]) {
		s = s[:i]
	}
	s = unescapeUnit(s)
	// Identificadores de dominio inverso: com.google.Chrome → Chrome.
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

// unescapeUnit deshace el escapado de systemd: "google\x2dchrome" → "google-chrome".
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

// containerName busca el nombre de un contenedor Docker en su configuración
// (requiere root; si no se puede leer, devuelve ""). Se guarda en caché.
func (r *Reader) containerName(id string) string {
	if n, ok := r.containerNames[id]; ok {
		return n
	}
	if r.containerNames == nil {
		r.containerNames = map[string]string{}
	}
	if len(r.containerNames) > 1000 { // los contenedores van y vienen: no crecer sin límite
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

	// El controlador de memoria puede no estar activado para este cgroup.
	if ev, err := readKeyValues(filepath.Join(path, "memory.events")); err == nil {
		svc.OOM = ev["oom"]
		svc.OOMKills = ev["oom_kill"]
	}
	svc.MemCurrent = readUint(filepath.Join(path, "memory.current"))
	svc.MemMax = readUint(filepath.Join(path, "memory.max")) // "max" se interpreta como 0
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
