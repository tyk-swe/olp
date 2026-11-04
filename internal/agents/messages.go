package agents

import (
	"slices"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
)

func resultIDs(result protocol.SendMessageResult) (string, string, bool) {
	switch v := result.(type) {
	case *protocol.Task:
		if v == nil || v.ID == "" || v.ContextID == "" {
			return "", "", false
		}
		return string(v.ID), v.ContextID, true
	case *protocol.Message:
		if v == nil || v.ID == "" {
			return "", "", false
		}
		return string(v.TaskID), v.ContextID, true
	default:
		return "", "", false
	}
}

func publicMessage(message *protocol.Message, id string) *protocol.Message {
	if message == nil {
		return nil
	}
	return &protocol.Message{ID: message.ID, ContextID: publicReference(message.ContextID, id), Extensions: message.Extensions, Metadata: message.Metadata, Parts: message.Parts, Role: message.Role, TaskID: protocol.TaskID(publicReference(string(message.TaskID), id))}
}

func publicReference(original, id string) string {
	if original == "" {
		return ""
	}
	return id
}

func publicTask(task *protocol.Task, id string) *protocol.Task {
	history := slices.Collect(func(yield func(*protocol.Message) bool) {
		for _, message := range task.History {
			if !yield(publicMessage(message, id)) {
				return
			}
		}
	})
	return &protocol.Task{ID: protocol.TaskID(id), ContextID: id, Artifacts: task.Artifacts, History: history, Metadata: task.Metadata, Status: protocol.TaskStatus{State: task.Status.State, Message: publicMessage(task.Status.Message, id), Timestamp: task.Status.Timestamp}}
}

func publicStreamEvent(event protocol.Event, id string) protocol.Event {
	switch value := event.(type) {
	case *protocol.Task:
		return publicTask(value, id)
	case *protocol.Message:
		return publicMessage(value, id)
	case *protocol.TaskStatusUpdateEvent:
		return &protocol.TaskStatusUpdateEvent{TaskID: protocol.TaskID(id), ContextID: id, Metadata: value.Metadata, Status: protocol.TaskStatus{State: value.Status.State, Message: publicMessage(value.Status.Message, id), Timestamp: value.Status.Timestamp}}
	case *protocol.TaskArtifactUpdateEvent:
		return &protocol.TaskArtifactUpdateEvent{TaskID: protocol.TaskID(id), ContextID: id, Artifact: value.Artifact, Append: value.Append, LastChunk: value.LastChunk, Metadata: value.Metadata}
	default:
		return nil
	}
}
func validInitialStreamEvent(event protocol.Event, continuation bool) bool {
	switch value := event.(type) {
	case *protocol.Task:
		return value != nil && value.ID != "" && value.ContextID != ""
	case *protocol.Message:
		return value != nil && value.ID != "" && (value.TaskID == "") == (value.ContextID == "")
	case *protocol.TaskStatusUpdateEvent:
		return continuation && value != nil && value.TaskID != "" && value.ContextID != ""
	case *protocol.TaskArtifactUpdateEvent:
		return continuation && value != nil && value.TaskID != "" && value.ContextID != ""
	default:
		return false
	}
}

func streamEventMatches(event protocol.Event, upstreamID, contextID string) bool {
	info := event.TaskInfo()
	return string(info.TaskID) == upstreamID && info.ContextID == contextID
}
