package a2a

import (
	"encoding/json"

	legacy "github.com/a2aproject/a2a-go/a2a"
	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/tyk-swe/olp/internal/agents"
)

type command interface{ command() }
type sendCommand struct {
	params    protocol.SendMessageRequest
	streaming bool
}
type getCommand struct{ params protocol.GetTaskRequest }
type cancelCommand struct{ params protocol.CancelTaskRequest }
type subscribeCommand struct {
	params protocol.SubscribeToTaskRequest
}

func (sendCommand) command()      {}
func (getCommand) command()       {}
func (cancelCommand) command()    {}
func (subscribeCommand) command() {}

func decodeRequest(version string, request rpcRequest) (command, error) {
	if version == currentProtocol {
		return decodeCurrent(request)
	}
	switch request.Method {
	case "message/send", "message/stream":
		var value legacy.MessageSendParams
		if json.Unmarshal(request.Params, &value) != nil || value.Message == nil || value.Config != nil && value.Config.PushConfig != nil {
			return nil, agents.ErrInvalidParameters
		}
		converted, err := a2av0.ToV1SendMessageRequest(&value)
		if err != nil {
			return nil, err
		}
		return sendCommand{params: *converted, streaming: request.Method == "message/stream"}, nil
	case "tasks/get":
		var value legacy.TaskQueryParams
		if json.Unmarshal(request.Params, &value) != nil {
			return nil, agents.ErrInvalidParameters
		}
		return getCommand{params: *a2av0.ToV1GetTaskRequest(&value)}, nil
	case "tasks/cancel", "tasks/resubscribe":
		var value legacy.TaskIDParams
		if json.Unmarshal(request.Params, &value) != nil {
			return nil, agents.ErrInvalidParameters
		}
		if request.Method == "tasks/cancel" {
			return cancelCommand{params: *a2av0.ToV1CancelTaskRequest(&value)}, nil
		}
		return subscribeCommand{params: *a2av0.ToV1SubscribeToTaskRequest(&value)}, nil
	default:
		return nil, errMethodNotDeclared
	}
}
func decodeCurrent(request rpcRequest) (command, error) {
	switch request.Method {
	case "SendMessage", "SendStreamingMessage":
		var value protocol.SendMessageRequest
		if json.Unmarshal(request.Params, &value) != nil {
			return nil, agents.ErrInvalidParameters
		}
		return sendCommand{params: value, streaming: request.Method == "SendStreamingMessage"}, nil
	case "GetTask":
		var value protocol.GetTaskRequest
		if json.Unmarshal(request.Params, &value) != nil {
			return nil, agents.ErrInvalidParameters
		}
		return getCommand{params: value}, nil
	case "CancelTask":
		var value protocol.CancelTaskRequest
		if json.Unmarshal(request.Params, &value) != nil {
			return nil, agents.ErrInvalidParameters
		}
		return cancelCommand{params: value}, nil
	case "SubscribeToTask":
		var value protocol.SubscribeToTaskRequest
		if json.Unmarshal(request.Params, &value) != nil {
			return nil, agents.ErrInvalidParameters
		}
		return subscribeCommand{params: value}, nil
	default:
		return nil, errMethodNotDeclared
	}
}
