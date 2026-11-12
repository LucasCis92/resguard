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

// El archivo de ejemplo que se instala debe ser válido y documentar
// exactamente los valores por defecto.
func TestExampleFileMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("deploy/config.yaml no es válido: %v", err)
	}
	def := Default()
	def.Services.Ignore = []string{} // el ejemplo lo escribe como lista vacía
	if !reflect.DeepEqual(cfg, def) {
		t.Errorf("deploy/config.yaml no coincide con los valores por defecto:\n%s\nquiero:\n%s", cfg.YAML(), def.YAML())
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
		t.Errorf("las claves ausentes deben tomar el valor por defecto: %+v", cfg)
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
		want       []string // fragmentos esperados en el mensaje
	}{
		{"clave mal escrita", "thresholds:\n  cpu: 80\n  memroy: 70\n", []string{"línea 3", `clave desconocida "memroy"`}},
		{"clave raíz desconocida", "intervalo: 2s\n", []string{`clave desconocida "intervalo"`}},
		{"tipo incorrecto", "thresholds:\n  cpu: mucho\n", []string{"línea 2", `"mucho" no es un número`}},
		{"duración inválida", "interval: rapido\n", []string{"línea 1", `"rapido" no es una duración`}},
		{"lista esperada", "services:\n  ignore: snap\n", []string{"línea 2", "lista"}},
		{"sintaxis rota", "thresholds: [sin cerrar\n", []string{"línea"}},
		{"intervalo demasiado corto", "interval: 10ms\n", []string{"interval"}},
		{"historial menor que el intervalo", "interval: 1m\nhistory: 30s\n", []string{"history"}},
		{"umbral fuera de rango", "thresholds:\n  cpu: 150\n", []string{"thresholds.cpu", "150"}},
		{"umbral negativo", "thresholds:\n  psi_io: -1\n", []string{"thresholds.psi_io"}},
		{"sustain cero", "thresholds:\n  sustain: 0\n", []string{"sustain"}},
		{"slice con ruta relativa", "services:\n  slices: [../../etc]\n", []string{"services.slices"}},
		{"slice absoluto", "services:\n  slices: [/sys/fs/cgroup]\n", []string{"services.slices"}},
		{"sin slices", "services:\n  slices: []\n", []string{"vacío"}},
		{"patrón inválido", "services:\n  ignore: [\"[\"]\n", []string{"services.ignore"}},
		{"varios errores a la vez", "interval: 1ms\nthresholds:\n  cpu: 200\n", []string{"interval", "thresholds.cpu"}},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.yaml))
		if err == nil {
			t.Errorf("%s: se esperaba un error", tt.name)
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: el error %q no menciona %q", tt.name, err, w)
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
			t.Errorf("Ignored(%q) = %v, quiero %v", name, got, want)
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
		t.Errorf("ida y vuelta: %+v, %v", back, err)
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
	// Chmod explícito: WriteFile y Mkdir aplican la umask.
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
		t.Errorf("archivo ausente opcional: %+v, %v, %v", cfg, found, err)
	}
	if _, _, err := Load(missing, false); err == nil {
		t.Error("un archivo ausente pedido explícitamente debe dar error")
	}

	bad := writeConfig(t, "thresholds:\n  cpu: 500\n", 0o644, 0o755)
	if _, _, err := Load(bad, false); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("el error debe indicar el archivo: %v", err)
	}
}

func TestLoadRejectsInsecurePermissions(t *testing.T) {
	tests := []struct {
		name             string
		fileMode, dirMod os.FileMode
	}{
		{"archivo escribible por todos", 0o666, 0o755},
		{"directorio escribible por todos", 0o644, 0o777},
	}
	for _, tt := range tests {
		path := writeConfig(t, "interval: 2s\n", tt.fileMode, tt.dirMod)
		_, _, err := Load(path, false)
		if err == nil || !strings.Contains(err.Error(), "otros usuarios") {
			t.Errorf("%s: se esperaba un rechazo por permisos, error = %v", tt.name, err)
		}
	}
}

// El grupo personal del dueño (habitual en Ubuntu, umask 002) es aceptable;
// otro grupo con permiso de escritura, no.
func TestLoadGroupWritable(t *testing.T) {
	path := writeConfig(t, "interval: 2s\n", 0o664, 0o775)
	if _, _, err := Load(path, false); err != nil {
		t.Fatalf("escribible solo por el grupo propio debería aceptarse: %v", err)
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
		if _, _, err := Load(path, false); err == nil || !strings.Contains(err.Error(), "otros usuarios") {
			t.Errorf("escribible por el grupo compartido %d debería rechazarse: %v", g, err)
		}
		return
	}
	t.Skip("el usuario no pertenece a otros grupos con los que probar")
}

func TestErrorsDoNotMentionGoTypes(t *testing.T) {
	for _, in := range []string{"thresholds:\n  memroy: 1\n", "thresholds:\n  cpu: x\n", "interval: x\n", "services:\n  ignore: x\n"} {
		_, err := Parse([]byte(in))
		if err == nil {
			t.Fatalf("Parse(%q) debería fallar", in)
		}
		for _, internal := range []string{"events.", "float64", "time.Duration", "[]string", "unmarshal", "yaml:"} {
			if strings.Contains(err.Error(), internal) {
				t.Errorf("el error de %q menciona %q: %v", in, internal, err)
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
			t.Errorf("formatDuration(%s) = %q, quiero %q", d, got, want)
		}
	}
}
