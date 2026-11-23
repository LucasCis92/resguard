// Package metrics reads system metrics from /proc and /sys/fs/cgroup.
//
// Readings are raw (cumulative counters); percentages that need two samples,
// such as CPU usage, are computed by separate functions.
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

// Reader reads metrics below a filesystem root. In production the root is
// "/"; tests point it at a fake tree. It is not safe for concurrent use.
type Reader struct {
	Root string

	containerNames map[string]string // cache: id → container name
	userNames      map[string]string // cache: uid → user name
}

// NewReader returns a reader for the real system.
func NewReader() *Reader { return &Reader{Root: "/"} }

func (r *Reader) path(p string) string { return filepath.Join(r.Root, p) }

// CPUTimes are the cumulative counters of the "cpu" line of /proc/stat, in ticks.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total returns the sum of all counters.
func (c CPUTimes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// IdleTotal returns the idle time, including I/O wait.
func (c CPUTimes) IdleTotal() uint64 { return c.Idle + c.IOWait }

// CPUPercent computes CPU usage between two readings.
func CPUPercent(prev, cur CPUTimes) float64 {
	total := float64(cur.Total()) - float64(prev.Total())
	idle := float64(cur.IdleTotal()) - float64(prev.IdleTotal())
	if total <= 0 {
		return 0
	}
	p := 100 * (total - idle) / total
	return clamp(p, 0, 100)
}

// Pressure is one line of /proc/pressure/* (PSI).
type Pressure struct {
	Avg10, Avg60, Avg300 float64
}

// PSI groups the "some" pressure of CPU, memory and I/O and the "full"
// pressure of memory and I/O.
// "some": share of time in which at least one task was stalled on the resource.
// "full": share of time in which all non-idle tasks were.
type PSI struct {
	Available          bool
	CPU, Memory, IO    Pressure // some
	MemoryFull, IOFull Pressure // full
}

// System is a snapshot of the overall system state.
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

// MemUsedPercent returns the percentage of memory in use.
func (s System) MemUsedPercent() float64 {
	if s.MemTotalKB == 0 {
		return 0
	}
	return 100 * float64(s.MemTotalKB-s.MemAvailableKB) / float64(s.MemTotalKB)
}

// SwapUsedPercent returns the percentage of swap in use.
func (s System) SwapUsedPercent() float64 {
	if s.SwapTotalKB == 0 {
		return 0
	}
	return 100 * float64(s.SwapTotalKB-s.SwapFreeKB) / float64(s.SwapTotalKB)
}

// ReadSystem takes a snapshot of the overall system state.
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
	// PSI is optional: it may be missing on old kernels or with psi=0.
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
				return c, fmt.Errorf("parsing /proc/stat: %w", err)
			}
		}
		return CPUTimes{vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], vals[6], vals[7]}, nil
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	return c, errors.New("parsing /proc/stat: missing cpu line")
}

func (r *Reader) readLoad(s *System) error {
	data, err := os.ReadFile(r.path("proc/loadavg"))
	if err != nil {
		return err
	}
	if _, err := fmt.Sscanf(string(data), "%f %f %f", &s.Load1, &s.Load5, &s.Load15); err != nil {
		return fmt.Errorf("parsing /proc/loadavg: %w", err)
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
		return errors.New("parsing /proc/meminfo: missing MemTotal")
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

// readPressure parses a PSI file with "some ..." and "full ..." lines.
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

// readKeyValues reads files with one "key value [unit]" per line, such as
// /proc/meminfo, memory.events or cpu.stat.
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
