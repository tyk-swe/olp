package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerinvoke"
)

func (s *Server) certifyToolCalls(ctx context.Context, cfg *Configuration, credential []byte, model string, stream bool) error {
	transport := cfg.transport()
	parsed, err := toolCallProbeRequest(transport, stream)
	if err != nil {
		return err
	}
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
	completion, err := toolCallProbeCompletion(body, stream, invocation, transport, parsed)
	if err != nil || completion == nil || len(completion.ToolCalls) != 1 || completion.ToolCalls[0].Name != "olp_certification_probe" || completion.ToolCalls[0].ID == "" || !json.Valid([]byte(completion.ToolCalls[0].Arguments)) {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not emit a valid requested tool call."}
	}
	return nil
}

func toolCallProbeRequest(transport connectors.Config, stream bool) (*wire.Request, error) {
	if transport.ProfileID != "" {
		target, err := transport.TargetFamily(wire.Chat)
		if err != nil {
			return nil, err
		}
		if target == wire.Responses {
			payload := fmt.Appendf(nil, `{"model":"certification","input":"Call the supplied function.","tools":[{"type":"function","name":"olp_certification_probe","parameters":{"type":"object","properties":{}}}],"tool_choice":{"type":"function","name":"olp_certification_probe"},"max_output_tokens":64,"store":false,"stream":%t}`, stream)
			return protocols.Parse(wire.Responses, payload, "certification")
		}
	}
	payload := fmt.Appendf(nil, `{"model":"certification","messages":[{"role":"user","content":"Call the supplied function."}],"tools":[{"type":"function","function":{"name":"olp_certification_probe","parameters":{"type":"object","properties":{}}}}],"tool_choice":{"type":"function","function":{"name":"olp_certification_probe"}},"max_tokens":64,"stream":%t}`, stream)
	return protocols.Parse(wire.Chat, payload, "certification")
}

func toolCallProbeCompletion(body []byte, stream bool, invocation providerinvoke.Invocation, transport connectors.Config, parsed *wire.Request) (*wire.Completion, error) {
	if !stream {
		return protocols.DecodeRequest(invocation.Wire, parsed.Family, transport.UnwrapResponse(body), "certification", "", parsed)
	}
	if parsed.Family == wire.Responses {
		completion, err := openai.Aggregate(wire.Responses, transport.StreamPayload(bytes.NewReader(body), probeBodyLimit), probeBodyLimit, probeBodyLimit)
		if err != nil {
			return nil, err
		}
		return openai.DecodeResponse(completion.Body, "certification")
	}
	var frames bytes.Buffer
	_, err := protocols.Stream(invocation.Wire, wire.Chat, transport.StreamPayload(bytes.NewReader(body), probeBodyLimit), probeBodyLimit, "certification", true, func(frame []byte) error {
		if frames.Len()+len(frame) > probeBodyLimit {
			return wire.ErrEventTooLarge
		}
		_, writeErr := frames.Write(frame)
		return writeErr
	})
	if err != nil {
		return nil, err
	}
	return openai.Stream(wire.Chat, bytes.NewReader(frames.Bytes()), probeBodyLimit, "certification", true, func([]byte) error { return nil })
}
