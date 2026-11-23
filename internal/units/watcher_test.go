package units

import (
	"testing"

	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/i18n"
)

func TestFailureEvent(t *testing.T) {
	en, es := i18n.New(i18n.English), i18n.New(i18n.Spanish)
	tests := []struct {
		props  map[string]interface{}
		en, es string
	}{
		{map[string]interface{}{"Result": "oom-kill"},
			"db.service went down (out of memory: killed by the OOM killer)",
			"db.service se cayó (sin memoria: lo mató el OOM killer)"},
		{map[string]interface{}{"Result": "exit-code", "ExecMainCode": int32(1), "ExecMainStatus": int32(3)},
			"db.service went down (exited with code 3)",
			"db.service se cayó (terminó con código 3)"},
		{map[string]interface{}{"Result": "signal", "ExecMainCode": int32(2), "ExecMainStatus": int32(9)},
			"db.service went down (received signal SIGKILL)",
			"db.service se cayó (recibió la señal SIGKILL)"},
		{map[string]interface{}{"Result": "core-dump", "ExecMainStatus": int32(11)},
			"db.service went down (received signal SIGSEGV and dumped core)",
			"db.service se cayó (recibió la señal SIGSEGV y generó un volcado de memoria)"},
		{map[string]interface{}{"Result": "signal", "ExecMainStatus": int32(42)},
			"db.service went down (received signal 42)",
			"db.service se cayó (recibió la señal 42)"},
		{map[string]interface{}{},
			"db.service went down (unknown reason)",
			"db.service se cayó (motivo desconocido)"},
		{map[string]interface{}{"ExecMainCode": int32(1), "ExecMainStatus": int32(2)},
			"db.service went down (exited with code 2)",
			"db.service se cayó (terminó con código 2)"},
		{map[string]interface{}{"Result": "exit-code", "ExecMainStatus": int32(1), "Restart": "on-failure", "NRestarts": uint32(4)},
			"db.service went down (exited with code 1); Restart=on-failure, previous restarts: 4",
			"db.service se cayó (terminó con código 1); Restart=on-failure, reinicios previos: 4"},
		{map[string]interface{}{"Result": "algo-nuevo", "Restart": "no"},
			"db.service went down (algo-nuevo)",
			"db.service se cayó (algo-nuevo)"},
	}
	for _, tt := range tests {
		e := failureEvent("db.service", tt.props)
		if e.Kind != events.ServiceFailed || e.Severity != events.Critical {
			t.Errorf("event = %+v", e)
		}
		if got := events.Describe(e, en); got != tt.en {
			t.Errorf("English: %q, want %q", got, tt.en)
		}
		if got := events.Describe(e, es); got != tt.es {
			t.Errorf("Spanish: %q, want %q", got, tt.es)
		}
	}
}
