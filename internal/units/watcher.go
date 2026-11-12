// Package units vigila las unidades de systemd por D-Bus y registra cuándo
// un servicio falla o es reiniciado automáticamente tras un fallo.
package units

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"

	"github.com/LucasCis92/resguard/internal/events"
)

// Bus indica a qué instancia de systemd conectarse.
type Bus string

const (
	SystemBus Bus = "system" // servicios del sistema (producción)
	UserBus   Bus = "user"   // servicios del usuario (desarrollo, systemd-run --user)
)

// Watcher escucha los cambios de estado de los servicios.
type Watcher struct {
	bus     Bus
	store   *events.Store
	log     *slog.Logger
	context func() *events.Context // estado del sistema para adjuntar a los eventos
	ignored func(unit string) bool // servicios excluidos de las incidencias

	state    map[string]unitState // último estado visto por unidad
	reported map[string]string    // última InvocationID notificada por unidad
}

type unitState struct{ active, sub string }

// NewWatcher crea un vigilante. contextFn e ignored pueden ser nil. ignored se
// consulta en cada caída, así que puede reflejar una configuración que cambia.
func NewWatcher(bus Bus, store *events.Store, log *slog.Logger, contextFn func() *events.Context, ignored func(string) bool) *Watcher {
	return &Watcher{bus: bus, store: store, log: log, context: contextFn, ignored: ignored,
		state: map[string]unitState{}, reported: map[string]string{}}
}

// Run vigila hasta que ctx se cancela, reconectando si se pierde la conexión.
func (w *Watcher) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := w.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("vigilancia de systemd interrumpida; reintentando", "bus", w.bus, "err", err, "en", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w *Watcher) connect(ctx context.Context) (*sd.Conn, error) {
	if w.bus == UserBus {
		return sd.NewUserConnectionContext(ctx)
	}
	return sd.NewSystemConnectionContext(ctx)
}

func (w *Watcher) watch(ctx context.Context) error {
	conn, err := w.connect(ctx)
	if err != nil {
		return fmt.Errorf("conectando con systemd: %w", err)
	}
	defer conn.Close()

	updates := make(chan *sd.PropertiesUpdate, 1024)
	errs := make(chan error, 16)
	conn.SetPropertiesSubscriber(updates, errs)
	if err := conn.Subscribe(); err != nil {
		return fmt.Errorf("suscribiéndose a systemd: %w", err)
	}

	// Estado inicial: los servicios que ya estaban caídos no son incidencias nuevas.
	units, err := conn.ListUnitsContext(ctx)
	if err != nil {
		return fmt.Errorf("listando unidades: %w", err)
	}
	for _, u := range units {
		if strings.HasSuffix(u.Name, ".service") {
			w.state[u.Name] = unitState{u.ActiveState, u.SubState}
		}
	}
	w.log.Info("vigilando servicios de systemd", "bus", w.bus, "servicios", len(w.state))

	// Si la conexión se cae, go-systemd cierra el canal interno pero no el
	// nuestro; comprobamos periódicamente que siga viva.
	alive := time.NewTicker(30 * time.Second)
	defer alive.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			w.log.Warn("error en la suscripción a systemd", "err", err)
		case <-alive.C:
			if !conn.Connected() {
				return fmt.Errorf("conexión con D-Bus perdida")
			}
		case u := <-updates:
			w.handle(ctx, conn, u)
		}
	}
}

func (w *Watcher) handle(ctx context.Context, conn *sd.Conn, u *sd.PropertiesUpdate) {
	if !strings.HasSuffix(u.UnitName, ".service") {
		return
	}
	prev := w.state[u.UnitName]
	cur := prev
	if v, ok := u.Changed["ActiveState"]; ok {
		cur.active = variantString(v)
	}
	if v, ok := u.Changed["SubState"]; ok {
		cur.sub = variantString(v)
	}
	w.state[u.UnitName] = cur
	if cur == prev {
		return
	}

	// Según la versión de systemd, una caída con Restart= pasa por "failed",
	// por "auto-restart" o por ambos; report evita duplicados por InvocationID.
	if (cur.sub == "auto-restart" && prev.sub != "auto-restart") ||
		(cur.active == "failed" && prev.active != "failed") {
		if w.ignored != nil && w.ignored(u.UnitName) {
			return
		}
		w.report(ctx, conn, u.UnitName)
	}
}

// report consulta por qué terminó el servicio y registra la incidencia,
// una sola vez por ejecución (InvocationID) del servicio.
func (w *Watcher) report(ctx context.Context, conn *sd.Conn, unit string) {
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if p, err := conn.GetUnitPropertyContext(qctx, unit, "InvocationID"); err == nil {
		if id, ok := p.Value.Value().([]byte); ok && len(id) > 0 {
			inv := fmt.Sprintf("%x", id)
			if w.reported[unit] == inv {
				return
			}
			w.reported[unit] = inv
		}
	}

	props, err := conn.GetUnitTypePropertiesContext(qctx, unit, "Service")
	if err != nil {
		w.log.Warn("consultando propiedades del servicio", "service", unit, "err", err)
		props = map[string]interface{}{}
	}

	result, _ := props["Result"].(string)
	msg := fmt.Sprintf("%s se cayó (%s)", unit, describe(result, props))
	if restart, _ := props["Restart"].(string); restart != "" && restart != "no" {
		msg += fmt.Sprintf("; Restart=%s", restart)
		if n, ok := props["NRestarts"].(uint32); ok {
			msg += fmt.Sprintf(", reinicios previos: %d", n)
		}
	}

	e := events.Event{
		Kind:     events.ServiceFailed,
		Severity: events.Critical,
		Service:  unit,
		Message:  msg,
	}
	if w.context != nil {
		e.Context = w.context()
	}
	w.store.Add(e)
}

// describe traduce el resultado de systemd a un texto legible.
func describe(result string, props map[string]interface{}) string {
	code, _ := props["ExecMainCode"].(int32)
	status, _ := props["ExecMainStatus"].(int32)

	switch result {
	case "oom-kill":
		return "sin memoria: lo mató el OOM killer"
	case "exit-code":
		return fmt.Sprintf("terminó con código %d", status)
	case "signal", "core-dump":
		s := fmt.Sprintf("recibió la señal %s", signalName(status))
		if result == "core-dump" {
			s += " y generó un volcado de memoria"
		}
		return s
	case "timeout":
		return "superó el tiempo de espera"
	case "watchdog":
		return "el watchdog dejó de recibir latidos"
	case "start-limit-hit":
		return "superó el límite de reinicios"
	case "resources":
		return "no se pudieron reservar los recursos necesarios"
	case "":
		// Sin propiedades (p. ej. la consulta falló): usamos el código de salida si lo hay.
		if code == 1 {
			return fmt.Sprintf("terminó con código %d", status)
		}
		return "motivo desconocido"
	default:
		return result
	}
}

func signalName(n int32) string {
	names := map[int32]string{1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 6: "SIGABRT", 9: "SIGKILL", 11: "SIGSEGV", 15: "SIGTERM"}
	if s, ok := names[n]; ok {
		return s
	}
	return fmt.Sprintf("%d", n)
}

func variantString(v godbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}
