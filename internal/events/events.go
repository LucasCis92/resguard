// Package events registra las incidencias detectadas: picos de recursos,
// OOM kills y caídas de servicios. Cada incidencia se escribe en el log del
// demonio (el journal, bajo systemd), se conserva en memoria para la CLI y,
// opcionalmente, se añade a un fichero JSON Lines que sobrevive a reinicios.
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
)

// Kind es el tipo de incidencia.
type Kind string

const (
	PeakStart     Kind = "peak_start"     // un recurso supera su umbral
	PeakEnd       Kind = "peak_end"       // el recurso vuelve a la normalidad
	OOMKill       Kind = "oom_kill"       // el OOM killer mató procesos de un servicio
	ServiceFailed Kind = "service_failed" // un servicio terminó con error
)

// Severity es la gravedad de una incidencia.
type Severity string

const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Event es una incidencia registrada.
type Event struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Kind     Kind      `json:"kind"`
	Severity Severity  `json:"severity"`
	Message  string    `json:"message"`

	Resource  string  `json:"resource,omitempty"`  // en picos: cpu, memory, psi_cpu…
	Service   string  `json:"service,omitempty"`   // en incidencias de servicios
	Value     float64 `json:"value,omitempty"`     // valor que disparó el evento o máximo del pico
	Threshold float64 `json:"threshold,omitempty"` // umbral superado
	Duration  string  `json:"duration,omitempty"`  // duración de un pico, al terminar

	Context *Context `json:"context,omitempty"` // estado del sistema en ese momento
}

// Context es una foto resumida del sistema que acompaña a una incidencia,
// para poder entender después qué estaba pasando.
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

// ServiceUsage es el consumo de un servicio dentro de un Context.
type ServiceUsage struct {
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	MemBytes   uint64  `json:"mem_bytes"`
}

// NewContext construye el contexto a partir de una muestra, con los topN
// servicios que más CPU y más memoria consumen.
func NewContext(s collector.Sample, services []collector.ServiceSample, topN int) *Context {
	ctx := &Context{
		CPUPercent:     s.CPUPercent,
		MemUsedPercent: s.MemUsedPercent,
		Load1:          s.Load1,
		PSICPU:         s.PSI.CPU.Avg10,
		PSIMemory:      s.PSI.Memory.Avg10,
		PSIIO:          s.PSI.IO.Avg10,
	}
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
		out[i] = ServiceUsage{Name: s.Name, CPUPercent: s.CPUPercent, MemBytes: s.MemBytes}
	}
	return out
}

// Store guarda las incidencias.
type Store struct {
	log *slog.Logger

	mu     sync.Mutex
	recent *collector.Ring[Event]
	nextID uint64
	file   *os.File // nil si no hay persistencia
}

// NewStore crea un almacén que conserva en memoria las últimas `keep`
// incidencias. Si path no está vacío, las añade a ese fichero y, al arrancar,
// recupera de él las más recientes.
func NewStore(path string, keep int, log *slog.Logger) (*Store, error) {
	s := &Store{log: log, recent: collector.NewRing[Event](keep), nextID: 1}
	if path == "" {
		return s, nil
	}
	if err := s.load(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creando directorio de eventos: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("abriendo fichero de eventos: %w", err)
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
		return fmt.Errorf("leyendo fichero de eventos: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			s.log.Warn("línea inválida en el fichero de eventos", "path", path, "line", line, "err", err)
			continue
		}
		s.recent.Push(e)
		if e.ID >= s.nextID {
			s.nextID = e.ID + 1
		}
	}
	return sc.Err()
}

// Add registra una incidencia: le asigna ID, la escribe en el log y la persiste.
func (s *Store) Add(e Event) Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	e.ID = s.nextID
	s.nextID++
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.recent.Push(e)
	s.logEvent(e)

	if s.file != nil {
		data, err := json.Marshal(e)
		if err == nil {
			_, err = s.file.Write(append(data, '\n'))
		}
		if err != nil {
			s.log.Error("guardando evento", "err", err)
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
			attrs = append(attrs, "top_cpu", c.TopCPU[0].Name)
		}
		if len(c.TopMemory) > 0 {
			attrs = append(attrs, "top_mem", c.TopMemory[0].Name)
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

// Recent devuelve hasta n incidencias, de la más antigua a la más reciente.
func (s *Store) Recent(n int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recent.Last(n)
}

// Close cierra el fichero de eventos.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
