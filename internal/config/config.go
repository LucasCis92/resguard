// Package config carga y valida el archivo de configuración de resguardd
// (por defecto /etc/resguard/config.yaml).
//
// Todas las claves son opcionales: lo que no aparece en el archivo toma el
// valor por defecto, y sin archivo el demonio funciona con los valores por defecto.
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

// DefaultPath es la ubicación por defecto del archivo.
const DefaultPath = "/etc/resguard/config.yaml"

// Config es la configuración del demonio.
type Config struct {
	Interval   time.Duration     `yaml:"interval"`
	History    time.Duration     `yaml:"history"`
	Thresholds events.Thresholds `yaml:"thresholds"`
	Services   Services          `yaml:"services"`
}

// Services indica qué servicios se vigilan.
type Services struct {
	// Slices de cgroups donde buscar servicios.
	Slices []string `yaml:"slices"`
	// Servicios que no generan incidencias. Admite comodines: "snap.*".
	Ignore []string `yaml:"ignore"`
}

// Default devuelve la configuración por defecto.
func Default() Config {
	return Config{
		Interval:   2 * time.Second,
		History:    time.Hour,
		Thresholds: events.DefaultThresholds,
		Services:   Services{Slices: []string{"system.slice"}},
	}
}

// Load lee el archivo de path sobre los valores por defecto. Si el archivo no
// existe y missingOK es true, devuelve los valores por defecto y found=false.
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

// Parse interpreta el contenido YAML sobre los valores por defecto y lo valida.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // una clave mal escrita es un error, no se ignora
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("formato inválido: %w", cleanYAMLError(err))
	}
	return cfg, cfg.Validate()
}

// Validate comprueba que los valores tengan sentido.
func (c Config) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Interval < 100*time.Millisecond || c.Interval > time.Hour {
		bad("interval debe estar entre 100ms y 1h (es %s)", c.Interval)
	}
	if c.History < c.Interval || c.History > 7*24*time.Hour {
		bad("history debe estar entre interval y 168h (es %s)", c.History)
	} else if c.Interval > 0 && c.History/c.Interval > 1_000_000 {
		bad("history/interval da más de 1.000.000 de muestras; aumenta interval o reduce history")
	}

	t := c.Thresholds
	for _, th := range []struct {
		name string
		v    float64
	}{{"cpu", t.CPU}, {"memory", t.Memory}, {"psi_cpu", t.PSICPU}, {"psi_memory", t.PSIMemory}, {"psi_io", t.PSIIO}} {
		if th.v < 0 || th.v > 100 {
			bad("thresholds.%s debe estar entre 0 (desactivado) y 100 (es %g)", th.name, th.v)
		}
	}
	if t.Sustain < 1 || t.Sustain > 1000 {
		bad("thresholds.sustain debe estar entre 1 y 1000 (es %d)", t.Sustain)
	}

	if len(c.Services.Slices) == 0 {
		bad("services.slices no puede estar vacío")
	}
	for _, s := range c.Services.Slices {
		if s == "" || filepath.IsAbs(s) || strings.Contains(s, "..") || !strings.HasSuffix(s, ".slice") && !strings.HasSuffix(s, ".service") {
			bad("services.slices: %q no es una ruta de cgroup válida (p. ej. system.slice)", s)
		}
	}
	for _, p := range c.Services.Ignore {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			bad("services.ignore: patrón inválido %q", p)
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Ignored indica si un servicio está excluido de las incidencias.
func (c Config) Ignored(service string) bool {
	for _, p := range c.Services.Ignore {
		if ok, _ := path.Match(p, service); ok {
			return true
		}
	}
	return false
}

// YAML devuelve la configuración en formato YAML, con duraciones legibles
// ("1h" en vez de "1h0m0s").
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

// checkPermissions rechaza el archivo si alguien que no sea su propietario
// (root o el usuario del demonio) puede modificarlo: podría, por ejemplo,
// desactivar todas las alertas. Es la misma comprobación que hace sshd.
func checkPermissions(f *os.File, path string) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s no es un archivo normal", path)
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
		return fmt.Errorf("%s pertenece a otro usuario (uid %d); debe pertenecer a root", name, st.Uid)
	}
	perm := fi.Mode().Perm()
	if fi.IsDir() && fi.Mode()&os.ModeSticky != 0 {
		return nil // como /tmp: otros pueden crear archivos, pero no modificar los ajenos
	}
	insecure := perm&0o002 != 0
	// Escribir por el grupo solo es aceptable si es el grupo propio del dueño
	// (root:root, o el grupo personal que crean Debian y Ubuntu para cada usuario).
	if perm&0o020 != 0 && st.Gid != primaryGID(st.Uid) {
		insecure = true
	}
	if insecure {
		return fmt.Errorf("%s puede ser modificado por otros usuarios (permisos %04o); corrígelo con: sudo chmod go-w %s", name, perm, name)
	}
	return nil
}

// primaryGID devuelve el grupo principal de un usuario, o un valor imposible
// si no se puede averiguar (así el archivo se considera inseguro).
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
	{regexp.MustCompile(`field (\S+) not found in type \S+`), `clave desconocida "$1" (¿está bien escrita?)`},
	{regexp.MustCompile("cannot unmarshal !!\\w+ `([^`]*)` into (float64|int)"), `"$1" no es un número`},
	{regexp.MustCompile("cannot unmarshal !!\\w+ `([^`]*)` into time.Duration"), `"$1" no es una duración (ejemplos: 500ms, 2s, 1m, 1h)`},
	{regexp.MustCompile("cannot unmarshal !!\\w+ `([^`]*)` into \\[\\]string"), `"$1" debería ser una lista, por ejemplo [a, b]`},
	{regexp.MustCompile(`line (\d+)`), "línea $1"},
}

// cleanYAMLError traduce los errores de la librería YAML a mensajes que no
// mencionen tipos internos de Go.
func cleanYAMLError(err error) error {
	msg := err.Error()
	for _, r := range yamlErrorRewrites {
		msg = r.re.ReplaceAllString(msg, r.repl)
	}
	return errors.New(msg)
}
