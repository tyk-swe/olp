package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerinvoke"
)

func (s *Server) certifyToolCalls(ctx context.Context, cfg *Configuration, credential []byte, model string, stream bool) error {
	payload := []byte(`{"model":"certification","messages":[{"role":"user","content":"Call the supplied function."}],"tools":[{"type":"function","function":{"name":"olp_certification_probe","parameters":{"type":"object","properties":{}}}}],"tool_choice":{"type":"function","function":{"name":"olp_certification_probe"}},"max_tokens":64}`)
	if stream {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			return err
		}
		fields["stream"] = json.RawMessage("true")
		var err error
		payload, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	parsed, err := protocols.Parse(wire.Chat, payload, "certification")
	if err != nil {
		return err
	}
	transport := cfg.transport()
	invocation, err := providerinvoke.Prepare(parsed, transport, model, cfg.Options.ParameterDefaults)
	if err != nil {
		return err
	}
	if transport.ForcesStreaming() && !stream {
		return &probeError{Code: "capability_unavailable", Detail: "A unary tool-call probe is unavailable."}
	}
	endpoint, err := transport.URL(invocation.Wire, model, stream)
	if err != nil {
		return err
	}
	status, body, err := s.call(ctx, cfg, credential, http.MethodPost, endpoint, transport.WrapRequest(invocation.Prepared.Document().Bytes(), model))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(cfg, status, body)
	}
	var completion *wire.Completion
	if stream {
		var frames bytes.Buffer
		_, err = protocols.Stream(invocation.Wire, wire.Chat, transport.StreamPayload(bytes.NewReader(body), probeBodyLimit), probeBodyLimit, "certification", true, func(frame []byte) error {
			if frames.Len()+len(frame) > probeBodyLimit {
				return wire.ErrEventTooLarge
			}
			_, writeErr := frames.Write(frame)
			return writeErr
		})
		if err == nil {
			completion, err = openai.Stream(wire.Chat, bytes.NewReader(frames.Bytes()), probeBodyLimit, "certification", true, func([]byte) error { return nil })
		}
	} else {
		completion, err = protocols.DecodeRequest(invocation.Wire, wire.Chat, transport.UnwrapResponse(body), "certification", "", parsed)
	}
	if err != nil || completion == nil || len(completion.ToolCalls) != 1 || completion.ToolCalls[0].Name != "olp_certification_probe" || completion.ToolCalls[0].ID == "" || !json.Valid([]byte(completion.ToolCalls[0].Arguments)) {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not emit a valid requested tool call."}
	}
	return nil
}
