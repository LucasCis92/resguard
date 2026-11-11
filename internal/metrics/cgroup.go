package metrics

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupRoot es la ruta de la jerarquía unificada de cgroups v2, relativa a Root.
const CgroupRoot = "sys/fs/cgroup"

// Service son los contadores del cgroup de un servicio de systemd.
type Service struct {
	Name         string // nombre de la unidad, p. ej. "nginx.service"
	Cgroup       string // ruta del cgroup relativa a la raíz de cgroups
	MemCurrent   uint64 // bytes en uso (memory.current)
	MemMax       uint64 // límite en bytes (memory.max); 0 = sin límite
	CPUUsageUsec uint64 // tiempo de CPU acumulado (cpu.stat usage_usec)
	OOM          uint64 // veces que se alcanzó el límite y actuó el OOM (memory.events oom)
	OOMKills     uint64 // procesos matados por el OOM killer (memory.events oom_kill)
}

// ReadServices recorre los slices indicados (p. ej. "system.slice") y devuelve
// los contadores de cada servicio que encuentre, incluidos los de slices anidados
// como system-getty.slice/getty@tty1.service.
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
			switch {
			case strings.HasSuffix(name, ".service"):
				rel, _ := filepath.Rel(base, path)
				if svc, ok := readService(path, name, rel); ok {
					out = append(out, svc)
				}
				return fs.SkipDir // los subcgroups de un servicio no son servicios
			case strings.HasSuffix(name, ".slice"):
				return nil // bajar a los slices anidados
			default:
				return fs.SkipDir
			}
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return out, err
		}
	}
	return out, nil
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
