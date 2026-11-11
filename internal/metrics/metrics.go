// Package metrics lee métricas del sistema desde /proc.
package metrics

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Snapshot es una foto del estado del sistema en un instante.
type Snapshot struct {
	Load1, Load5, Load15 float64
	MemTotalKB           uint64
	MemAvailableKB       uint64
}

// MemUsedPercent devuelve el porcentaje de memoria en uso.
func (s Snapshot) MemUsedPercent() float64 {
	if s.MemTotalKB == 0 {
		return 0
	}
	return 100 * float64(s.MemTotalKB-s.MemAvailableKB) / float64(s.MemTotalKB)
}

// Read toma una foto del estado actual del sistema.
func Read() (Snapshot, error) {
	var s Snapshot

	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return s, err
	}
	if _, err := fmt.Sscanf(string(data), "%f %f %f", &s.Load1, &s.Load5, &s.Load15); err != nil {
		return s, fmt.Errorf("interpretando /proc/loadavg: %w", err)
	}

	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return s, err
	}
	defer f.Close()

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
		switch fields[0] {
		case "MemTotal:":
			s.MemTotalKB = v
		case "MemAvailable:":
			s.MemAvailableKB = v
		}
	}
	return s, sc.Err()
}
