package events

import (
	"testing"

	"github.com/LucasCis92/resguard/internal/i18n"
)

func TestDescribe(t *testing.T) {
	en, es := i18n.New(i18n.English), i18n.New(i18n.Spanish)
	tests := []struct {
		e      Event
		en, es string
	}{
		{Event{Kind: PeakStart, Resource: "cpu", Value: 96, Threshold: 90},
			"CPU usage peak: 96.0% (threshold 90%)",
			"pico de uso de CPU: 96.0% (umbral 90%)"},
		{Event{Kind: PeakEnd, Resource: "psi_io", Value: 55.5, Duration: "14s"},
			"I/O pressure peak ended after 14s (max 55.5%)",
			"fin del pico de presión de E/S tras 14s (máximo 55.5%)"},
		{Event{Kind: PeakEnd, Resource: "memory", Value: 93, Duration: "1m0s", Reason: "disabled"},
			"memory usage peak closed after 1m0s because its alert was disabled in the configuration (max 93.0%)",
			"pico de uso de memoria cerrado tras 1m0s porque su alerta se desactivó en la configuración (máximo 93.0%)"},
		{Event{Kind: OOMKill, Service: "app-x-1.scope", ServiceLabel: "Chrome", Value: 2, MemBytes: 300 << 20, MemMax: 512 << 20},
			"the OOM killer killed 2 process(es) of Chrome (using 300.0 MiB of a 512.0 MiB limit)",
			"el OOM killer mató 2 proceso(s) de Chrome (uso actual 300.0 MiB de un límite de 512.0 MiB)"},
		{Event{Kind: OOMKill, Service: "db.service", Value: 1},
			"the OOM killer killed 1 process(es) of db.service",
			"el OOM killer mató 1 proceso(s) de db.service"},
		{Event{Kind: ServiceFailed, Service: "db.service", Reason: "exit-code", ExitStatus: 3},
			"db.service went down (exited with code 3)",
			"db.service se cayó (terminó con código 3)"},
		// Recorded before 0.1.0: no structured reason, only a Spanish message.
		{Event{Kind: ServiceFailed, Service: "db.service", Message: "db.service se cayó (terminó con código 3)"},
			"db.service se cayó (terminó con código 3)",
			"db.service se cayó (terminó con código 3)"},
	}
	for _, tt := range tests {
		if got := Describe(tt.e, en); got != tt.en {
			t.Errorf("English:\n got  %q\n want %q", got, tt.en)
		}
		if got := Describe(tt.e, es); got != tt.es {
			t.Errorf("Spanish:\n got  %q\n want %q", got, tt.es)
		}
	}
}

func TestStoreWritesEnglishMessage(t *testing.T) {
	s := newStore(t)
	e := s.Add(Event{Kind: PeakStart, Severity: Warning, Resource: "memory", Value: 91, Threshold: 90})
	if e.Message != "memory usage peak: 91.0% (threshold 90%)" {
		t.Errorf("stored message = %q", e.Message)
	}
}

func TestLabels(t *testing.T) {
	es := i18n.New(i18n.Spanish)
	if got := KindLabel("container", es); got != "contenedor" {
		t.Errorf("KindLabel = %q", got)
	}
	if got := SeverityLabel(Critical, es); got != "CRÍTICO" {
		t.Errorf("SeverityLabel = %q", got)
	}
	if got := KindLabel("container", i18n.Printer{}); got != "container" {
		t.Errorf("KindLabel in English = %q", got)
	}
}
