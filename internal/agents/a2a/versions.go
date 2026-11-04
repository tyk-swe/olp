package a2a

import (
	"encoding/json"
	"net/http"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
)

const legacyProtocol = "a2a-0.3.0"
const currentProtocol = "a2a-1.0.0"

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
