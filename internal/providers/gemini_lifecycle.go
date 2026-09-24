package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

func (s *Server) certifyGeminiInteraction(ctx context.Context, cfg *Configuration, credential []byte, model, mode string, maxEventBytes int) error {
	if mode != ModeUnary && mode != ModeStreaming {
		return &probeError{Code: "capability_unavailable", Detail: "Gemini Interactions profile supports unary and streaming calls."}
	}
	endpoint, err := cfg.transport().InteractionsURL("", "")
	if err != nil {
		return &probeError{Code: "invalid_endpoint", Detail: "The Interactions profile cannot address this endpoint."}
	}
	upstream := cfg.transport().Model(model)
	if !connectors.ModelValid(KindGemini, upstream) {
		return &probeError{Code: "model_invalid", Detail: "The Interactions model binding is malformed."}
	}
	body, _ := json.Marshal(map[string]any{"model": upstream, "input": "Reply with OK.", "store": false,
		"stream": mode == ModeStreaming, "generation_config": map[string]any{"max_output_tokens": 16}})
	status, result, err := s.call(ctx, cfg, credential, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(status)
	}
	if mode == ModeUnary {
		if _, err := geminilifecycle.ParseInteractionResponse(result, probeBodyLimit); err != nil {
			return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not return a valid Interaction resource."}
		}
		return nil
	}
	state := &geminilifecycle.InteractionStream{}
	err = sse.Decode(bytes.NewReader(result), maxEventBytes, func(frame sse.Frame) error {
		event, err := geminilifecycle.ParseInteractionEvent([]byte(frame.Data), maxEventBytes)
		if err != nil {
			return err
		}
		return state.Accept(event)
	})
	if err != nil || !state.Done {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not return a complete Interaction event stream."}
	}
	return nil
}

func (s *Server) certifyGeminiLive(ctx context.Context, cfg *Configuration, credential []byte, model string, maxEventBytes int) error {
	endpoint, err := cfg.transport().GeminiLiveURL()
	if err != nil {
		return &probeError{Code: "invalid_endpoint", Detail: "The Live profile cannot address this endpoint."}
	}
	check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
	if _, err := s.Egress.ValidateEndpoint(check); err != nil {
		return &probeError{Code: "invalid_endpoint", Detail: "The Live endpoint is not permitted by egress policy."}
	}
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	case <-ctx.Done():
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &probeError{Code: "invalid_endpoint", Detail: "The Live endpoint is malformed."}
	}
	transport := cfg.transport()
	if _, err := s.auth.Apply(ctx, req, transport, credential, nil); err != nil {
		return &probeError{Code: "credential_invalid", Detail: "The Live credential could not be applied."}
	}
	client, err := s.connectionClient(ctx, cfg, credential)
	if err != nil {
		return &probeError{Code: "network_credential_invalid", Detail: "The Live network credential is unavailable."}
	}
	conn, response, err := websocket.Dial(ctx, req.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: req.Header})
	if err != nil {
		if response != nil {
			return statusError(response.StatusCode)
		}
		return classify(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(int64(maxEventBytes))
	upstream := transport.Model(model)
	if !connectors.ModelValid(KindGemini, upstream) {
		return &probeError{Code: "model_invalid", Detail: "The Live model binding is malformed."}
	}
	setup, _ := json.Marshal(map[string]any{"setup": map[string]any{"model": "models/" + strings.TrimPrefix(upstream, "models/")}})
	if err := conn.Write(ctx, websocket.MessageText, setup); err != nil {
		return classify(err)
	}
	_, responseBody, err := conn.Read(ctx)
	if err != nil || geminilifecycle.ValidateLiveServerFrame(responseBody, maxEventBytes) != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The Live provider did not acknowledge setup."}
	}
	doc, err := oif.ParseJSON(responseBody, oif.Limits{MaxBytes: maxEventBytes})
	if err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The Live provider returned malformed setup acknowledgement."}
	}
	if _, ok := doc.Root().Lookup("setupComplete"); !ok {
		return &probeError{Code: "provider_protocol_error", Detail: "The Live provider did not acknowledge setup."}
	}
	return nil
}
