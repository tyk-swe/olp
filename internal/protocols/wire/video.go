package wire

type VideoJobState string

const (
	VideoJobQueued    VideoJobState = "queued"
	VideoJobRunning   VideoJobState = "running"
	VideoJobSucceeded VideoJobState = "succeeded"
	VideoJobFailed    VideoJobState = "failed"
)

func VideoJobStateOf(status string) (VideoJobState, bool) {
	switch status {
	case "queued":
		return VideoJobQueued, true
	case "in_progress":
		return VideoJobRunning, true
	case "completed":
		return VideoJobSucceeded, true
	case "failed":
		return VideoJobFailed, true
	default:
		return "", false
	}
}
