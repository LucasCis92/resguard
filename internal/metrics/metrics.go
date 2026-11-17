// Package metrics lee métricas del sistema desde /proc y /sys/fs/cgroup.
//
// Las lecturas son "en bruto" (contadores acumulados); los porcentajes que
// necesitan dos muestras, como el uso de CPU, se calculan con funciones aparte.
package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reader lee métricas a partir de una raíz del sistema de ficheros.
// En producción la raíz es "/"; en los tests apunta a testdata.
// No es seguro para uso concurrente.
type Reader struct {
	Root string

	containerNames map[string]string // caché id → nombre de contenedor
}

// NewReader devuelve un lector sobre el sistema real.
func NewReader() *Reader { return &Reader{Root: "/"} }

func (r *Reader) path(p string) string { return filepath.Join(r.Root, p) }

// CPUTimes son los contadores acumulados de la línea "cpu" de /proc/stat, en ticks.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total devuelve la suma de todos los contadores.
func (c CPUTimes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// IdleTotal devuelve el tiempo ocioso (incluida la espera de E/S).
func (c CPUTimes) IdleTotal() uint64 { return c.Idle + c.IOWait }

// CPUPercent calcula el uso de CPU entre dos lecturas.
func CPUPercent(prev, cur CPUTimes) float64 {
	total := float64(cur.Total()) - float64(prev.Total())
	idle := float64(cur.IdleTotal()) - float64(prev.IdleTotal())
	if total <= 0 {
		return 0
	}
	p := 100 * (total - idle) / total
	return clamp(p, 0, 100)
}

// Pressure es una línea de /proc/pressure/* (PSI).
type Pressure struct {
	Avg10, Avg60, Avg300 float64
}

// PSI agrupa la presión "some" de CPU, memoria y E/S, y la "full" de memoria y E/S.
// "some": porcentaje de tiempo en que al menos una tarea estuvo bloqueada por el recurso.
// "full": porcentaje de tiempo en que todas las tareas no ociosas lo estuvieron.
type PSI struct {
	Available          bool
	CPU, Memory, IO    Pressure // some
	MemoryFull, IOFull Pressure // full
}

// System es una foto del estado general del sistema.
type System struct {
	Time                 time.Time
	CPU                  CPUTimes
	Load1, Load5, Load15 float64
	MemTotalKB           uint64
	MemAvailableKB       uint64
	SwapTotalKB          uint64
	SwapFreeKB           uint64
	PSI                  PSI
}

// MemUsedPercent devuelve el porcentaje de memoria en uso.
func (s System) MemUsedPercent() float64 {
	if s.MemTotalKB == 0 {
		return 0
	}
	return 100 * float64(s.MemTotalKB-s.MemAvailableKB) / float64(s.MemTotalKB)
}

// SwapUsedPercent devuelve el porcentaje de swap en uso.
func (s System) SwapUsedPercent() float64 {
	if s.SwapTotalKB == 0 {
		return 0
	}
	return 100 * float64(s.SwapTotalKB-s.SwapFreeKB) / float64(s.SwapTotalKB)
}

// ReadSystem toma una foto del estado general del sistema.
func (r *Reader) ReadSystem() (System, error) {
	s := System{Time: time.Now()}
	var err error

	if s.CPU, err = r.readCPU(); err != nil {
		return s, err
	}
	if err = r.readLoad(&s); err != nil {
		return s, err
	}
	if err = r.readMem(&s); err != nil {
		return s, err
	}
	// PSI es opcional: puede no existir en kernels antiguos o con psi=0.
	s.PSI, err = r.readPSI()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	return s, nil
}

func (r *Reader) readCPU() (CPUTimes, error) {
	var c CPUTimes
	f, err := os.Open(r.path("proc/stat"))
	if err != nil {
		return c, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 9 || fields[0] != "cpu" {
			continue
		}
		vals := make([]uint64, 8)
		for i := range vals {
			if vals[i], err = strconv.ParseUint(fields[i+1], 10, 64); err != nil {
				return c, fmt.Errorf("interpretando /proc/stat: %w", err)
			}
		}
		return CPUTimes{vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], vals[6], vals[7]}, nil
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	return c, errors.New("interpretando /proc/stat: falta la línea cpu")
}

func (r *Reader) readLoad(s *System) error {
	data, err := os.ReadFile(r.path("proc/loadavg"))
	if err != nil {
		return err
	}
	if _, err := fmt.Sscanf(string(data), "%f %f %f", &s.Load1, &s.Load5, &s.Load15); err != nil {
		return fmt.Errorf("interpretando /proc/loadavg: %w", err)
	}
	return nil
}

func (r *Reader) readMem(s *System) error {
	vals, err := readKeyValues(r.path("proc/meminfo"))
	if err != nil {
		return err
	}
	s.MemTotalKB = vals["MemTotal:"]
	s.MemAvailableKB = vals["MemAvailable:"]
	s.SwapTotalKB = vals["SwapTotal:"]
	s.SwapFreeKB = vals["SwapFree:"]
	if s.MemTotalKB == 0 {
		return errors.New("interpretando /proc/meminfo: falta MemTotal")
	}
	return nil
}

func (r *Reader) readPSI() (PSI, error) {
	var p PSI
	var err error
	if p.CPU, _, err = readPressure(r.path("proc/pressure/cpu")); err != nil {
		return p, err
	}
	if p.Memory, p.MemoryFull, err = readPressure(r.path("proc/pressure/memory")); err != nil {
		return p, err
	}
	if p.IO, p.IOFull, err = readPressure(r.path("proc/pressure/io")); err != nil {
		return p, err
	}
	p.Available = true
	return p, nil
}

// readPressure interpreta un fichero PSI con líneas "some ..." y "full ...".
func readPressure(path string) (some, full Pressure, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return some, full, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var p Pressure
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			f, perr := strconv.ParseFloat(v, 64)
			if perr != nil {
				continue
			}
			switch k {
			case "avg10":
				p.Avg10 = f
			case "avg60":
				p.Avg60 = f
			case "avg300":
				p.Avg300 = f
			}
		}
		switch fields[0] {
		case "some":
			some = p
		case "full":
			full = p
		}
	}
	return some, full, nil
}

// readKeyValues lee ficheros con formato "clave valor [unidad]" por línea,
// como /proc/meminfo, memory.events o cpu.stat.
func readKeyValues(path string) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vals := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		vals[fields[0]] = v
	}
	return vals, sc.Err()
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
