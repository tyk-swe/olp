package probe

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

func (p *Prober) certifyGeminiInteraction(ctx context.Context, t Target, model, mode string, maxEventBytes int) error {
	if mode != "unary" && mode != "streaming" {
		return &Failure{Reason: CapabilityUnavailable, Detail: "Gemini Interactions profile supports unary and streaming calls."}
	}
	endpoint, err := t.Connection.Transport().InteractionsURL("", "")
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The Interactions profile cannot address this endpoint."}
	}
	upstream := t.Connection.Transport().Model(model)
	if !connectors.ModelValid(connectors.KindGemini, upstream) {
		return &Failure{Reason: ModelInvalid, Detail: "The Interactions model binding is malformed."}
	}
	body, _ := json.Marshal(map[string]any{"model": upstream, "input": "Reply with OK.", "store": false,
		"stream": mode == "streaming", "generation_config": map[string]any{"max_output_tokens": 16}})
	status, result, err := p.call(ctx, t, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(t, status, result)
	}
	if mode == "unary" {
		if _, err := geminilifecycle.ParseInteractionResponse(result, BodyLimit); err != nil {
			return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not return a valid Interaction resource."}
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
		return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not return a complete Interaction event stream."}
	}
	return nil
}

func (p *Prober) certifyGeminiLive(ctx context.Context, t Target, model string, maxEventBytes int) error {
	endpoint, err := t.Connection.Transport().GeminiLiveURL()
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The Live profile cannot address this endpoint."}
	}
	check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
	if _, err := p.egress.ValidateEndpoint(check); err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The Live endpoint is not permitted by egress policy."}
	}
	ctx, release, err := p.slot(ctx)
	if err != nil {
		return err
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The Live endpoint is malformed."}
	}
	transport := t.Connection.Transport()
	if _, err := p.auth.Apply(ctx, req, transport, t.Credential, nil); err != nil {
		return &Failure{Reason: CredentialInvalid, Detail: "The Live credential could not be applied."}
	}
	client, err := p.Client(ctx, t)
	if err != nil {
		return &Failure{Reason: NetworkCredentialInvalid, Detail: "The Live network credential is unavailable."}
	}
	conn, response, err := websocket.Dial(ctx, req.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: req.Header})
	if err != nil {
		if response != nil {
			return statusError(t, response.StatusCode, nil)
		}
		return Classify(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(int64(maxEventBytes))
	upstream := transport.Model(model)
	if !connectors.ModelValid(connectors.KindGemini, upstream) {
		return &Failure{Reason: ModelInvalid, Detail: "The Live model binding is malformed."}
	}
	setup, _ := json.Marshal(map[string]any{"setup": map[string]any{"model": "models/" + strings.TrimPrefix(upstream, "models/")}})
	if err := conn.Write(ctx, websocket.MessageText, setup); err != nil {
		return Classify(err)
	}
	_, responseBody, err := conn.Read(ctx)
	if err != nil || geminilifecycle.ValidateLiveServerFrame(responseBody, maxEventBytes) != nil {
		return &Failure{Reason: ProviderProtocolError, Detail: "The Live provider did not acknowledge setup."}
	}
	doc, err := oif.ParseJSON(responseBody, oif.Limits{MaxBytes: maxEventBytes})
	if err != nil {
		return &Failure{Reason: ProviderProtocolError, Detail: "The Live provider returned malformed setup acknowledgement."}
	}
	if _, ok := doc.Root().Lookup("setupComplete"); !ok {
		return &Failure{Reason: ProviderProtocolError, Detail: "The Live provider did not acknowledge setup."}
	}
	return nil
}
