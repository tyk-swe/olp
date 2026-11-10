package wire

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	protocolwire "github.com/tyk-swe/olp/internal/protocols/wire"
)

func RewriteRequestModel(body []byte, from, to string) ([]byte, error) {
	return rewriteModel(body, from, to, func(oif.Document) string { return "/model" })
}

func RewriteResponseModel(body []byte, from, to string) ([]byte, error) {
	return rewriteModel(body, from, to, func(oif.Document) string { return "/model" })
}

func RewriteEventModel(body []byte, protocol code.Protocol, from, to string) ([]byte, error) {
	return rewriteModel(body, from, to, func(document oif.Document) string { return eventModelPointer(document, protocol) })
}

func eventModelPointer(document oif.Document, protocol code.Protocol) string {
	kind, _ := document.Lookup("/type")
	name, _ := kind.Text()
	switch protocol {
	case code.ProtocolMessages:
		if name == "message_start" {
			return "/message/model"
		}
	case code.ProtocolResponses:
		if protocolwire.IsResponseLifecycleEvent(name) {
			return "/response/model"
		}
	case code.ProtocolChat:
		return "/model"
	}
	return ""
}

type modelPatch struct {
	start, end  int
	replacement []byte
}

func RewriteEventFrame(frame sse.RawFrame, protocol code.Protocol, from, to string) ([]byte, error) {
	if !frame.HasData {
		return frame.Bytes, nil
	}
	patch, err := modelChange(frame.Data, from, to, func(document oif.Document) string { return eventModelPointer(document, protocol) })
	if err != nil {
		return nil, err
	}
	if patch == nil {
		return frame.Bytes, nil
	}
	return frame.ReplaceData(patch.start, patch.end, patch.replacement)
}

func rewriteModel(body []byte, from, to string, pointer func(oif.Document) string) ([]byte, error) {
	patch, err := modelChange(body, from, to, pointer)
	if err != nil {
		return nil, err
	}
	if patch == nil {
		return body, nil
	}
	return slices.Concat(body[:patch.start], patch.replacement, body[patch.end:]), nil
}

func modelChange(body []byte, from, to string, pointer func(oif.Document) string) (*modelPatch, error) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return nil, code.Refuse(400, "code_body_invalid")
	}
	path := pointer(document)
	if path == "" {
		return nil, nil
	}
	value, ok := document.Lookup(path)
	model, text := value.Text()
	if !ok || !text || model != from || from == to {
		return nil, nil
	}
	start, end := value.Bounds()
	replacement, _ := json.Marshal(to)
	return &modelPatch{start: start, end: end, replacement: replacement}, nil
}
