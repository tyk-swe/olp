package wire

import "strings"

func IsResponseLifecycleEvent(kind string) bool {
	switch kind {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

func IsTerminalResponseEvent(kind string) bool {
	return IsResponseLifecycleEvent(kind) && ResponseStatusFinished(strings.TrimPrefix(kind, "response."))
}

func ResponseStatusFinished(status string) bool {
	switch status {
	case "completed", "incomplete", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func ValidResponseStatus(status string) bool {
	switch status {
	case "queued", "in_progress", "cancelling":
		return true
	default:
		return ResponseStatusFinished(status)
	}
}
