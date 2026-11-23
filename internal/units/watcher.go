// Package units watches systemd units over D-Bus and records when a service
// fails or is automatically restarted after a failure.
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

// Bus selects the systemd instance to connect to.
type Bus string

const (
	SystemBus Bus = "system" // system services (production)
	UserBus   Bus = "user"   // the user's services (development, systemd-run --user)
)

// Watcher listens for service state changes.
type Watcher struct {
	bus     Bus
	store   *events.Store
	log     *slog.Logger
	context func() *events.Context // system state to attach to events
	ignored func(unit string) bool // units excluded from incidents

	state    map[string]unitState // last state seen per unit
	reported map[string]string    // last reported InvocationID per unit
}

type unitState struct{ active, sub string }

// NewWatcher creates a watcher. contextFn and ignored may be nil. ignored is
// called on every failure, so it can reflect a configuration that changes.
func NewWatcher(bus Bus, store *events.Store, log *slog.Logger, contextFn func() *events.Context, ignored func(string) bool) *Watcher {
	return &Watcher{bus: bus, store: store, log: log, context: contextFn, ignored: ignored,
		state: map[string]unitState{}, reported: map[string]string{}}
}

// Run watches until ctx is canceled, reconnecting if the connection is lost.
func (w *Watcher) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := w.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("systemd watch interrupted; retrying", "bus", w.bus, "err", err, "in", backoff)
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
		return fmt.Errorf("connecting to systemd: %w", err)
	}
	defer conn.Close()

	updates := make(chan *sd.PropertiesUpdate, 1024)
	errs := make(chan error, 16)
	conn.SetPropertiesSubscriber(updates, errs)
	if err := conn.Subscribe(); err != nil {
		return fmt.Errorf("subscribing to systemd: %w", err)
	}

	// Initial state: services that were already down are not new incidents.
	units, err := conn.ListUnitsContext(ctx)
	if err != nil {
		return fmt.Errorf("listing units: %w", err)
	}
	for _, u := range units {
		if strings.HasSuffix(u.Name, ".service") {
			w.state[u.Name] = unitState{u.ActiveState, u.SubState}
		}
	}
	w.log.Info("watching systemd services", "bus", w.bus, "services", len(w.state))

	// If the connection drops, go-systemd closes its internal channel but not
	// ours, so check periodically that it is still alive.
	alive := time.NewTicker(30 * time.Second)
	defer alive.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			w.log.Warn("systemd subscription error", "err", err)
		case <-alive.C:
			if !conn.Connected() {
				return fmt.Errorf("D-Bus connection lost")
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

	// Depending on the systemd version, a failure with Restart= goes through
	// "failed", "auto-restart" or both; report avoids duplicates by InvocationID.
	if (cur.sub == "auto-restart" && prev.sub != "auto-restart") ||
		(cur.active == "failed" && prev.active != "failed") {
		if w.ignored != nil && w.ignored(u.UnitName) {
			return
		}
		w.report(ctx, conn, u.UnitName)
	}
}

// report asks systemd why the service ended and records the incident, once
// per run (InvocationID) of the service.
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
		w.log.Warn("querying service properties", "service", unit, "err", err)
		props = map[string]interface{}{}
	}

	e := failureEvent(unit, props)
	if w.context != nil {
		e.Context = w.context()
	}
	w.store.Add(e)
}

// failureEvent builds the incident from the service's D-Bus properties.
func failureEvent(unit string, props map[string]interface{}) events.Event {
	result, _ := props["Result"].(string)
	code, _ := props["ExecMainCode"].(int32)
	status, _ := props["ExecMainStatus"].(int32)
	if result == "" {
		// No properties (e.g. the query failed): use the exit code if there is one.
		result = "unknown"
		if code == 1 { // CLD_EXITED
			result = "exit-code"
		}
	}
	e := events.Event{
		Kind:       events.ServiceFailed,
		Severity:   events.Critical,
		Service:    unit,
		Reason:     result,
		ExitStatus: int(status),
	}
	if restart, _ := props["Restart"].(string); restart != "" && restart != "no" {
		e.Restart = restart
		if n, ok := props["NRestarts"].(uint32); ok {
			e.Restarts = int(n)
		}
	}
	return e
}

func variantString(v godbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}
