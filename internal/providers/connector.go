package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Probe bounds: one upstream call, one response body, four in flight.
const (
	probeTimeout   = 15 * time.Second
	probeBodyLimit = 1 << 20
)

// probeError is a classified, content-free upstream failure.
type probeError struct {
	Code   string
	Detail string
}

func (e *probeError) Error() string { return e.Detail }

func classify(err error) *probeError {
	var pe *probeError
	if errors.As(err, &pe) {
		return pe
	}
	switch {
	case errors.Is(err, egress.ErrUnsafeDestination):
		return &probeError{Code: "unsafe_destination", Detail: "The endpoint resolves to a blocked address."}
	case errors.Is(err, egress.ErrRedirect):
		return &probeError{Code: "upstream_redirect", Detail: "The upstream answered with a redirect, which the gateway refuses."}
	case errors.Is(err, context.DeadlineExceeded):
		return &probeError{Code: "gateway_timeout", Detail: "The upstream did not answer within the probe deadline."}
	case errors.Is(err, context.Canceled):
		return &probeError{Code: "client_cancelled", Detail: "The probe was cancelled."}
	}
	return &probeError{Code: "upstream_unavailable", Detail: "The upstream could not be reached."}
}

// call performs one bounded upstream request. The body is capped and never
// retained beyond the caller's parsing.
func (s *Server) call(ctx context.Context, cfg *Configuration, credential []byte, method, path string, body []byte) (int, []byte, error) {
	base, err := s.Egress.ValidateEndpoint(*cfg.Endpoint)
	if err != nil {
		return 0, nil, &probeError{Code: "invalid_endpoint", Detail: err.Error()}
	}
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String()+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "olp-go/probe")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := cfg.applyCredential(req, credential); err != nil {
		return 0, nil, &probeError{Code: "credential_invalid", Detail: err.Error()}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, classify(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit+1))
	if err != nil {
		return resp.StatusCode, nil, classify(err)
	}
	if len(data) > probeBodyLimit {
		return resp.StatusCode, nil, &probeError{Code: "upstream_response_too_large", Detail: "The upstream response exceeded the probe body limit."}
	}
	return resp.StatusCode, data, nil
}

// statusError uses only local codes and text: upstream error fields may echo
// credentials and must never enter persistent probe diagnostics.
func statusError(status int) *probeError {
	detail := fmt.Sprintf("The upstream answered HTTP %d.", status)
	code := "upstream_rejected"
	switch {
	case status == 401:
		code, detail = "upstream_authentication_failed", "The upstream rejected the credential (HTTP 401)."
	case status == 403:
		code, detail = "upstream_permission_denied", "The upstream denied access (HTTP 403)."
	case status == 404:
		code, detail = "upstream_not_found", "The upstream has no such resource (HTTP 404)."
	case status == 429:
		code, detail = "upstream_rate_limit", "The upstream is rate limiting (HTTP 429)."
	case status >= 500:
		code, detail = "upstream_unavailable", fmt.Sprintf("The upstream failed (HTTP %d).", status)
	}
	return &probeError{Code: code, Detail: detail}
}

// listModels asks the upstream for its model identifiers.
func (s *Server) listModels(ctx context.Context, cfg *Configuration, credential []byte) ([]string, error) {
	status, body, err := s.call(ctx, cfg, credential, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(status)
	}
	return decodeModels(body)
}

func decodeModels(body []byte) ([]string, error) {
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil || listing.Data == nil {
		return nil, &probeError{Code: "provider_protocol_error", Detail: "The upstream model listing must contain a data array."}
	}
	var models []string
	seen := map[string]bool{}
	for _, m := range listing.Data {
		if validModelName("model", m.ID) != nil || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		models = append(models, m.ID)
		if len(models) == 2000 {
			break
		}
	}
	return models, nil
}

// certifyTuple performs the smallest real generation for one capability.
func (s *Server) certifyTuple(ctx context.Context, cfg *Configuration, credential []byte, model string, mode string, maxEventBytes int) error {
	payload := map[string]any{"model": "certification", "messages": []map[string]string{{"role": "user", "content": "Reply with OK."}}}
	if mode == ModeStreaming {
		payload["stream"] = true
		payload["stream_options"] = map[string]bool{"include_usage": true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	parsed, err := openai.Parse(openai.FamilyChat, body)
	if err != nil {
		return err
	}
	body, err = parsed.Encode(model, cfg.Options.ParameterDefaults)
	if err != nil {
		return err
	}
	status, data, err := s.call(ctx, cfg, credential, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(status)
	}
	if mode == ModeStreaming {
		_, err = openai.Stream(openai.FamilyChat, bytes.NewReader(data), maxEventBytes, model, true, func([]byte) error { return nil })
	} else {
		_, err = openai.DecodeChat(data, model)
	}
	if err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream answered with a response the gateway could not decode as OpenAI " + mode + " output."}
	}
	return nil
}

// credentialFor loads the default slot's plaintext credential for probes.
func (s *Server) credentialFor(ctx context.Context, tx pgx.Tx, p *record) ([]byte, *credentialState, error) {
	state := &credentialState{}
	err := tx.QueryRow(ctx, "SELECT c.id::text,c.version,c.revoked_at IS NOT NULL FROM olp_go.provider_slots s JOIN olp_go.provider_credentials c ON c.id=s.credential_id WHERE s.provider_id=$1 AND s.is_default", p.ID).Scan(&state.ID, &state.Version, &state.Revoked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	if !p.Configuration.credentialRequired() {
		return nil, state, nil
	}
	if state.ID == nil {
		return nil, state, access.Fail(422, "credential_required", "Add a credential before probing this connection.")
	}
	if state.Revoked {
		return nil, state, access.Fail(422, "credential_revoked", "The draft credential was revoked; rotate before probing.")
	}
	secret, err := s.Access.Keys.Read(ctx, tx, s.Access.Installation, *state.ID, "provider_credential")
	if err != nil {
		return nil, state, err
	}
	return secret, state, nil
}
