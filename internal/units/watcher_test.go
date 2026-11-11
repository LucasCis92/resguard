package units

import "testing"

func TestDescribe(t *testing.T) {
	tests := []struct {
		result string
		props  map[string]interface{}
		want   string
	}{
		{"oom-kill", nil, "sin memoria: lo mató el OOM killer"},
		{"exit-code", map[string]interface{}{"ExecMainCode": int32(1), "ExecMainStatus": int32(3)}, "terminó con código 3"},
		{"signal", map[string]interface{}{"ExecMainCode": int32(2), "ExecMainStatus": int32(9)}, "recibió la señal SIGKILL"},
		{"core-dump", map[string]interface{}{"ExecMainStatus": int32(11)}, "recibió la señal SIGSEGV y generó un volcado de memoria"},
		{"signal", map[string]interface{}{"ExecMainStatus": int32(42)}, "recibió la señal 42"},
		{"", map[string]interface{}{}, "motivo desconocido"},
		{"algo-nuevo", nil, "algo-nuevo"},
	}
	for _, tt := range tests {
		if got := describe(tt.result, tt.props); got != tt.want {
			t.Errorf("describe(%q) = %q, quiero %q", tt.result, got, tt.want)
		}
	}
}
