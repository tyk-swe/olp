package a2a

import (
	"encoding/json"
	"errors"
	"net/http"

	legacy "github.com/a2aproject/a2a-go/a2a"
	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
)

const legacyProtocol = "a2a-0.3.0"
const currentProtocol = "a2a-1.0.0"

var errMethodNotDeclared = errors.New("method not declared")

func supportedProtocol(version string) bool {
	return version == legacyProtocol || version == currentProtocol
}

func requestProtocol(r *http.Request) string {
	header := r.Header.Get("A2A-Version")
	query := r.URL.Query().Get("A2A-Version")
	if header != "" && query != "" && protocolVersion(header) != protocolVersion(query) {
		return ""
	}
	if header != "" {
		return protocolVersion(header)
	}
	if query != "" {
		return protocolVersion(query)
	}
	return legacyProtocol
}

func protocolVersion(version string) string {
	switch version {
	case string(a2av0.Version), "0.3.0":
		return legacyProtocol
	case string(protocol.Version), "1.0.0":
		return currentProtocol
	default:
		return ""
	}
}

func decodeRequest(version string, request rpcRequest) (rpcRequest, error) {
	if version == currentProtocol {
		switch request.Method {
		case "SendMessage", "GetTask", "CancelTask", "SendStreamingMessage", "SubscribeToTask":
			return request, nil
		default:
			return rpcRequest{}, errMethodNotDeclared
		}
	}
	switch request.Method {
	case "message/send", "message/stream":
		var value legacy.MessageSendParams
		if json.Unmarshal(request.Params, &value) != nil || value.Message == nil || value.Config != nil && value.Config.PushConfig != nil {
			return rpcRequest{}, errors.New("invalid parameters")
		}
		converted, err := a2av0.ToV1SendMessageRequest(&value)
		if err != nil {
			return rpcRequest{}, err
		}
		data, err := json.Marshal(converted)
		if err != nil {
			return rpcRequest{}, err
		}
		return rpcRequest{JSONRPC: request.JSONRPC, ID: request.ID, Method: messageMethod(request.Method), Params: data}, nil
	case "tasks/get":
		var value legacy.TaskQueryParams
		if json.Unmarshal(request.Params, &value) != nil {
			return rpcRequest{}, errors.New("invalid parameters")
		}
		data, err := json.Marshal(a2av0.ToV1GetTaskRequest(&value))
		return rpcRequest{JSONRPC: request.JSONRPC, ID: request.ID, Method: "GetTask", Params: data}, err
	case "tasks/resubscribe":
		var value legacy.TaskIDParams
		if json.Unmarshal(request.Params, &value) != nil {
			return rpcRequest{}, errors.New("invalid parameters")
		}
		data, err := json.Marshal(a2av0.ToV1SubscribeToTaskRequest(&value))
		return rpcRequest{JSONRPC: request.JSONRPC, ID: request.ID, Method: "SubscribeToTask", Params: data}, err
	case "tasks/cancel":
		var value legacy.TaskIDParams
		if json.Unmarshal(request.Params, &value) != nil {
			return rpcRequest{}, errors.New("invalid parameters")
		}
		data, err := json.Marshal(a2av0.ToV1CancelTaskRequest(&value))
		return rpcRequest{JSONRPC: request.JSONRPC, ID: request.ID, Method: "CancelTask", Params: data}, err
	default:
		return rpcRequest{}, errMethodNotDeclared
	}
}

func messageMethod(method string) string {
	if method == "message/stream" {
		return "SendStreamingMessage"
	}
	return "SendMessage"
}

func marshalEvent(version string, event protocol.Event) ([]byte, error) {
	if version == legacyProtocol {
		converted, err := a2av0.FromV1Event(event)
		if err != nil {
			return nil, err
		}
		return json.Marshal(converted)
	}
	return json.Marshal(protocol.StreamResponse{Event: event})
}

func marshalTask(version string, task *protocol.Task) ([]byte, error) {
	if version == legacyProtocol {
		return json.Marshal(a2av0.FromV1Task(task))
	}
	return json.Marshal(task)
}
