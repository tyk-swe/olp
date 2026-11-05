package agents

import (
	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/agents/tasks"
)

func taskEventState(event protocol.Event) tasks.State {
	switch value := event.(type) {
	case *protocol.Task:
		if value != nil {
			return taskState(value.Status.State)
		}
	case *protocol.TaskStatusUpdateEvent:
		if value != nil {
			return taskState(value.Status.State)
		}
	}
	return "active"
}

func taskState(state protocol.TaskState) tasks.State {
	switch state {
	case protocol.TaskStateCompleted:
		return "completed"
	case protocol.TaskStateCanceled:
		return "canceled"
	case protocol.TaskStateFailed, protocol.TaskStateRejected:
		return "failed"
	case protocol.TaskStateSubmitted, protocol.TaskStateWorking, protocol.TaskStateInputRequired, protocol.TaskStateAuthRequired:
		return "active"
	default:
		return "outcome_unknown"
	}
}
