package events

import (
	"fmt"

	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/metrics"
)

// Describe writes an incident as a sentence in p's language.
//
// Incidents recorded by versions before 0.1.0 only have a (Spanish) Message
// for service failures; for those, Message is returned as is.
func Describe(e Event, p i18n.Printer) string {
	switch e.Kind {
	case PeakStart:
		return p.T("%s peak: %.1f%% (threshold %.0f%%)", ResourceLabel(e.Resource, p), e.Value, e.Threshold)
	case PeakEnd:
		if e.Reason == "disabled" {
			return p.T("%s peak closed after %s because its alert was disabled in the configuration (max %.1f%%)",
				ResourceLabel(e.Resource, p), e.Duration, e.Value)
		}
		return p.T("%s peak ended after %s (max %.1f%%)", ResourceLabel(e.Resource, p), e.Duration, e.Value)
	case OOMKill:
		unit := e.ServiceLabel
		if unit == "" {
			unit = e.Service
		}
		switch {
		case e.MemBytes > 0 && e.MemMax > 0:
			return p.T("the OOM killer killed %d process(es) of %s (using %s of a %s limit)",
				int(e.Value), unit, FormatBytes(e.MemBytes), FormatBytes(e.MemMax))
		case e.MemBytes > 0:
			return p.T("the OOM killer killed %d process(es) of %s (using %s)", int(e.Value), unit, FormatBytes(e.MemBytes))
		default:
			return p.T("the OOM killer killed %d process(es) of %s", int(e.Value), unit)
		}
	case ServiceFailed:
		if e.Reason == "" && e.Message != "" {
			return e.Message
		}
		s := p.T("%s went down (%s)", e.Service, failureReason(e, p))
		if e.Restart != "" {
			s += p.T("; Restart=%s, previous restarts: %d", e.Restart, e.Restarts)
		}
		return s
	}
	return e.Message
}

// ResourceLabel names a monitored resource ("cpu" → "CPU usage").
func ResourceLabel(resource string, p i18n.Printer) string {
	switch resource {
	case "cpu":
		return p.T("CPU usage")
	case "memory":
		return p.T("memory usage")
	case "psi_cpu":
		return p.T("CPU pressure")
	case "psi_memory":
		return p.T("memory pressure")
	case "psi_io":
		return p.T("I/O pressure")
	}
	return resource
}

// failureReason explains systemd's Result for a failed service.
func failureReason(e Event, p i18n.Printer) string {
	switch e.Reason {
	case "oom-kill":
		return p.T("out of memory: killed by the OOM killer")
	case "exit-code":
		return p.T("exited with code %d", e.ExitStatus)
	case "signal":
		return p.T("received signal %s", SignalName(e.ExitStatus))
	case "core-dump":
		return p.T("received signal %s and dumped core", SignalName(e.ExitStatus))
	case "timeout":
		return p.T("timed out")
	case "watchdog":
		return p.T("the watchdog stopped receiving keep-alives")
	case "start-limit-hit":
		return p.T("hit the restart limit")
	case "resources":
		return p.T("could not allocate the required resources")
	case "unknown":
		return p.T("unknown reason")
	}
	return e.Reason
}

// SignalName returns the name of common signals ("SIGKILL"), or the number.
func SignalName(n int) string {
	names := map[int]string{1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 6: "SIGABRT", 9: "SIGKILL", 11: "SIGSEGV", 15: "SIGTERM"}
	if s, ok := names[n]; ok {
		return s
	}
	return fmt.Sprintf("%d", n)
}

// FormatBytes formats a size with binary units ("1.5 GiB").
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// KindLabel names a unit kind (metrics.Kind*) in p's language.
func KindLabel(kind string, p i18n.Printer) string {
	switch kind {
	case metrics.KindService:
		return p.T("service")
	case metrics.KindApp:
		return p.T("app")
	case metrics.KindContainer:
		return p.T("container")
	case metrics.KindSession:
		return p.T("session")
	case metrics.KindOther:
		return p.T("other")
	}
	return kind
}

// SeverityLabel names a severity in capitals ("CRITICAL").
func SeverityLabel(s Severity, p i18n.Printer) string {
	switch s {
	case Critical:
		return p.T("CRITICAL")
	case Warning:
		return p.T("WARNING")
	}
	return p.T("INFO")
}
