// Package events records detected incidents: resource peaks, OOM kills and
// service failures. Each incident is written to the daemon log (the journal,
// under systemd), kept in memory for the CLI and, optionally, appended to a
// JSON Lines file that survives restarts.
//
// Incidents are stored as structured data; Describe turns them into a
// sentence in the reader's language. Message holds the English sentence, for
// the log and for anyone reading the JSON file directly.
package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/i18n"
)

// Kind is the type of incident.
type Kind string

const (
	PeakStart     Kind = "peak_start"     // a resource went over its threshold
	PeakEnd       Kind = "peak_end"       // the resource is back to normal
	OOMKill       Kind = "oom_kill"       // the OOM killer killed processes of a unit
	ServiceFailed Kind = "service_failed" // a service exited with an error
)

// Severity is how serious an incident is.
type Severity string

const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Event is a recorded incident.
type Event struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Kind     Kind      `json:"kind"`
	Severity Severity  `json:"severity"`
	Message  string    `json:"message"` // English sentence; see Describe

	Resource     string  `json:"resource,omitempty"`      // peaks: cpu, memory, psi_cpu…
	Service      string  `json:"service,omitempty"`       // unit name, for unit incidents
	ServiceLabel string  `json:"service_label,omitempty"` // readable unit name (Chrome…)
	Value        float64 `json:"value,omitempty"`         // value that fired the event, peak maximum or OOM kill count
	Threshold    float64 `json:"threshold,omitempty"`     // threshold that was crossed
	Duration     string  `json:"duration,omitempty"`      // duration of a finished peak

	// Why a peak ended early ("disabled") or why a service failed (systemd's
	// Result: "exit-code", "signal", "oom-kill"…).
	Reason     string `json:"reason,omitempty"`
	ExitStatus int    `json:"exit_status,omitempty"` // exit code, or signal number for "signal"/"core-dump"
	Restart    string `json:"restart,omitempty"`     // Restart= policy, when it is not "no"
	Restarts   int    `json:"restarts,omitempty"`    // restarts before this failure
	MemBytes   uint64 `json:"mem_bytes,omitempty"`   // OOM kills: the unit's memory usage
	MemMax     uint64 `json:"mem_max,omitempty"`     // OOM kills: the unit's memory limit

	Context *Context `json:"context,omitempty"` // system state at that moment
}

// Context is a summary of the system state attached to an incident, so that
// what was going on can be understood later.
type Context struct {
	CPUPercent     float64        `json:"cpu_percent"`
	MemUsedPercent float64        `json:"mem_used_percent"`
	Load1          float64        `json:"load1"`
	PSICPU         float64        `json:"psi_cpu_avg10"`
	PSIMemory      float64        `json:"psi_memory_avg10"`
	PSIIO          float64        `json:"psi_io_avg10"`
	TopCPU         []ServiceUsage `json:"top_cpu,omitempty"`
	TopMemory      []ServiceUsage `json:"top_memory,omitempty"`
}

// ServiceUsage is a unit's usage inside a Context.
type ServiceUsage struct {
	Name       string  `json:"name"`
	Display    string  `json:"display,omitempty"`
	Kind       string  `json:"kind,omitempty"`
	CPUPercent float64 `json:"cpu_percent"`
	MemBytes   uint64  `json:"mem_bytes"`
}

// Label returns the readable name, or the unit name if there is none.
func (u ServiceUsage) Label() string {
	if u.Display != "" {
		return u.Display
	}
	return u.Name
}

// NewContext builds a context from a sample, with the topN units using the
// most CPU and the most memory.
func NewContext(s collector.Sample, services []collector.ServiceSample, topN int) *Context {
	ctx := &Context{
		CPUPercent:     s.CPUPercent,
		MemUsedPercent: s.MemUsedPercent,
		Load1:          s.Load1,
		PSICPU:         s.PSI.CPU.Avg10,
		PSIMemory:      s.PSI.Memory.Avg10,
		PSIIO:          s.PSI.IO.Avg10,
	}
	services = collector.Grouped(services)
	ctx.TopCPU = top(services, topN, func(a, b collector.ServiceSample) bool { return a.CPUPercent > b.CPUPercent })
	ctx.TopMemory = top(services, topN, func(a, b collector.ServiceSample) bool { return a.MemBytes > b.MemBytes })
	return ctx
}

func top(services []collector.ServiceSample, n int, less func(a, b collector.ServiceSample) bool) []ServiceUsage {
	sorted := append([]collector.ServiceSample(nil), services...)
	sort.SliceStable(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	out := make([]ServiceUsage, len(sorted))
	for i, s := range sorted {
		out[i] = ServiceUsage{Name: s.Name, Display: s.Display, Kind: s.Kind, CPUPercent: s.CPUPercent, MemBytes: s.MemBytes}
	}
	return out
}

// Store keeps the incidents.
type Store struct {
	log *slog.Logger

	mu     sync.Mutex
	recent *collector.Ring[Event]
	nextID uint64
	file   *os.File // nil without persistence
}

// NewStore creates a store that keeps the last `keep` incidents in memory. If
// path is not empty, incidents are appended to that file, and the most recent
// ones are loaded from it at startup.
func NewStore(path string, keep int, log *slog.Logger) (*Store, error) {
	s := &Store{log: log, recent: collector.NewRing[Event](keep), nextID: 1}
	if path == "" {
		return s, nil
	}
	if err := s.load(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating the events directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening the events file: %w", err)
	}
	s.file = f
	return s, nil
}

func (s *Store) load(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the events file: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			s.log.Warn("invalid line in the events file", "path", path, "line", line, "err", err)
			continue
		}
		s.recent.Push(e)
		if e.ID >= s.nextID {
			s.nextID = e.ID + 1
		}
	}
	return sc.Err()
}

// Add records an incident: it assigns an ID, writes the English message,
// logs it and persists it.
func (s *Store) Add(e Event) Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	e.ID = s.nextID
	s.nextID++
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Message == "" {
		e.Message = Describe(e, i18n.New(i18n.English))
	}
	s.recent.Push(e)
	s.logEvent(e)

	if s.file != nil {
		data, err := json.Marshal(e)
		if err == nil {
			_, err = s.file.Write(append(data, '\n'))
		}
		if err != nil {
			s.log.Error("saving event", "err", err)
		}
	}
	return e
}

func (s *Store) logEvent(e Event) {
	attrs := []any{"event_id", e.ID, "kind", string(e.Kind)}
	if e.Resource != "" {
		attrs = append(attrs, "resource", e.Resource)
	}
	if e.Service != "" {
		attrs = append(attrs, "service", e.Service)
	}
	if e.Value != 0 {
		attrs = append(attrs, "value", round1(e.Value))
	}
	if e.Duration != "" {
		attrs = append(attrs, "duration", e.Duration)
	}
	if c := e.Context; c != nil {
		attrs = append(attrs, "cpu", round1(c.CPUPercent), "mem", round1(c.MemUsedPercent))
		if len(c.TopCPU) > 0 {
			attrs = append(attrs, "top_cpu", c.TopCPU[0].Label())
		}
		if len(c.TopMemory) > 0 {
			attrs = append(attrs, "top_mem", c.TopMemory[0].Label())
		}
	}
	switch e.Severity {
	case Critical:
		s.log.Error(e.Message, attrs...)
	case Warning:
		s.log.Warn(e.Message, attrs...)
	default:
		s.log.Info(e.Message, attrs...)
	}
}

// Recent returns up to n incidents, oldest first.
func (s *Store) Recent(n int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recent.Last(n)
}

// Close closes the events file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
