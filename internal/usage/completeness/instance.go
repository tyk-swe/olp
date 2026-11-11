package completeness

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// sanitizeHost keeps the host recognizable while guaranteeing the name is safe
// in a Valkey key, a log line and a report: lowercase alphanumerics, dashes and
// underscores survive, everything else becomes a dash, and the result is
// bounded so one long hostname cannot bloat every pending entry.
func sanitizeHost(host string) string {
	var out strings.Builder
	for _, r := range host {
		if out.Len() == 48 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r + ('a' - 'A'))
		default:
			out.WriteByte('-')
		}
	}
	return out.String()
}

// GatewayInstance distinguishes live processes even when they share a host
// or have no HOSTNAME. Restarts leave their old epochs for stale detection.
func GatewayInstance(hostname string) string {
	return ProcessName(gatewayInstanceLabel(hostname), os.Getpid(), uuid.Must(uuid.NewV7()))
}

func gatewayInstanceLabel(host string) string {
	var out strings.Builder
	for _, r := range host {
		if r == '�' || unicode.IsControl(r) {
			continue
		}
		if out.Len()+len(string(r)) > 200 {
			break
		}
		out.WriteRune(r)
	}
	label := strings.TrimSpace(out.String())
	if label == "" {
		return "olp"
	}
	return label
}

func ProcessName(host string, pid int, epoch uuid.UUID) string {
	label := sanitizeHost(host)
	if label == "" {
		label = "olp"
	}
	return fmt.Sprintf("%s-%d-%s", label, pid, hex.EncodeToString(epoch[:]))
}
