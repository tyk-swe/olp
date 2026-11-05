package a2a

import (
	"encoding/json"
	"fmt"
	"net/http"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
)

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
}

func writeStreamEvent(w http.ResponseWriter, id json.RawMessage, event protocol.Event, version string) bool {
	public := event
	result, err := marshalEvent(version, public)
	if err != nil || public == nil {
		writeStreamFailure(w, id, "Invalid upstream stream")
		return false
	}
	response, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: responseID(id), Result: result})
	if err != nil || len(response)+8 > maxRPCBytes {
		writeStreamFailure(w, id, "Stream event exceeds one MiB")
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", response); err != nil {
		return false
	}
	w.(http.Flusher).Flush()
	return true
}

func writeStreamFailure(w http.ResponseWriter, id json.RawMessage, message string) {
	response, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: responseID(id), Error: &rpcError{Code: -32603, Message: message}})
	if _, err := fmt.Fprintf(w, "data: %s\n\n", response); err == nil {
		w.(http.Flusher).Flush()
	}
}
