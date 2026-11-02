package wire

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
