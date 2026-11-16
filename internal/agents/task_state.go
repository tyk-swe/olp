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
	return tasks.Active
}

func taskState(state protocol.TaskState) tasks.State {
	switch state {
	case protocol.TaskStateCompleted:
		return tasks.Completed
	case protocol.TaskStateCanceled:
		return tasks.Canceled
	case protocol.TaskStateFailed, protocol.TaskStateRejected:
		return tasks.Failed
	case protocol.TaskStateSubmitted, protocol.TaskStateWorking, protocol.TaskStateInputRequired, protocol.TaskStateAuthRequired:
		return tasks.Active
	default:
		return tasks.Unknown
	}
}
