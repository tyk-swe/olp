package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols"
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
	if pe, ok := errors.AsType[*probeError](err); ok {
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
	endpoint := base.String() + path
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		endpoint = path
		u, e := url.Parse(endpoint)
		if e != nil {
			return 0, nil, e
		}
		u.RawQuery = ""
		if _, e := s.Egress.ValidateEndpoint(u.String()); e != nil {
			return 0, nil, e
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if cfg.Kind == KindBedrock && strings.HasSuffix(path, "/converse-stream") {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	}
	req.Header.Set("User-Agent", "olp-go/probe")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if _, err := s.auth.Apply(ctx, req, cfg.transport(), credential, body); err != nil {
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
	models, err := s.listModelFacts(ctx, cfg, credential)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m.Name)
	}
	return ids, nil
}

type discoveredModel struct {
	Name, Display string
	Metadata      map[string]json.RawMessage
}

func (s *Server) listModelFacts(ctx context.Context, cfg *Configuration, credential []byte) ([]discoveredModel, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	discovery := true
	for _, vendor := range vendors {
		if vendor.ID == value(cfg.Options.VendorID) {
			discovery = vendor.Discovery
		}
	}
	if cfg.Kind == KindAzure || cfg.Kind == KindVertex || !discovery {
		names := append([]string{}, cfg.ProbeModels...)
		if cfg.Kind == KindAzure {
			names = append(names, value(cfg.Deployment))
		}
		for name := range cfg.Options.Models {
			names = append(names, name)
		}
		slices.Sort(names)
		if len(names) == 0 {
			return nil, &probeError{Code: "model_required", Detail: "Declare a model before probing this vendor."}
		}
		out := []discoveredModel{}
		previous := ""
		for _, name := range names {
			if name == previous {
				continue
			}
			previous = name
			operation := "generation"
			if cfg.Kind == KindVertex {
				operation = "token_count"
			}
			if value(cfg.Options.VendorID) == "voyage" {
				operation = "embeddings"
			}
			tuple := CapabilityInput{Operation: operation, Surface: "openai", Mode: "unary"}
			err := s.certifyTuple(ctx, cfg, credential, name, tuple, probeBodyLimit)
			if err != nil && cfg.Kind == KindAzure {
				tuple.Operation = "embeddings"
				err = s.certifyTuple(ctx, cfg, credential, name, tuple, probeBodyLimit)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, discoveredModel{Name: name, Display: name})
		}
		return out, nil
	}
	basePath := "/models"
	key, idKey, displayKey := "data", "id", "display_name"
	if cfg.Kind == KindGemini {
		key, idKey, displayKey = "models", "name", "displayName"
	}
	if cfg.Kind == KindBedrock {
		key, idKey, displayKey = "modelSummaries", "modelId", "modelName"
		basePath = "/foundation-models"
		if value(cfg.Endpoint) == connectors.DefaultEndpoint(cfg.Kind, value(cfg.CloudRegion), "") {
			endpoint, err := connectors.BedrockEndpoint(value(cfg.CloudRegion), true)
			if err != nil {
				return nil, err
			}
			basePath = endpoint + "/foundation-models"
		}
	}
	out := []discoveredModel{}
	seen, cursors := map[string]bool{}, map[string]bool{}
	path := basePath
	for {
		status, body, err := s.call(ctx, cfg, credential, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, statusError(status)
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(body, &envelope) != nil {
			return nil, &probeError{Code: "provider_protocol_error", Detail: "Invalid model discovery response."}
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(envelope[key], &items) != nil || items == nil {
			return nil, &probeError{Code: "provider_protocol_error", Detail: "Discovery response is missing its model array."}
		}
		for _, item := range items {
			var name, display string
			_ = json.Unmarshal(item[idKey], &name)
			_ = json.Unmarshal(item[displayKey], &display)
			name = strings.TrimPrefix(name, "models/")
			if ValidModelName("model", name) != nil || seen[name] {
				continue
			}
			seen[name] = true
			if display == "" {
				display = name
			}
			facts := map[string]json.RawMessage{}
			for from, to := range map[string]string{"inputTokenLimit": "context_length", "outputTokenLimit": "max_output_tokens", "inputModalities": "input_modalities", "outputModalities": "output_modalities"} {
				if v, ok := item[from]; ok {
					facts[to] = v
				}
			}
			if len(facts) > 0 {
				facts["source"], _ = json.Marshal(value(cfg.Endpoint))
				facts["observed_at"], _ = json.Marshal(time.Now().UTC())
			}
			out = append(out, discoveredModel{Name: name, Display: display, Metadata: facts})
			if len(out) == 2000 {
				return out, nil
			}
		}
		cursor, parameter := "", ""
		if cfg.Kind == KindGemini {
			_ = json.Unmarshal(envelope["nextPageToken"], &cursor)
			parameter = "pageToken"
		}
		if cfg.Kind == KindAnthropic {
			var more bool
			_ = json.Unmarshal(envelope["has_more"], &more)
			if more {
				_ = json.Unmarshal(envelope["last_id"], &cursor)
				if cursor == "" {
					return nil, &probeError{Code: "provider_protocol_error", Detail: "Discovery continuation is missing its cursor."}
				}
			}
			parameter = "after_id"
		}
		if cursor == "" {
			return out, nil
		}
		if len(cursor) > 2048 || cursors[cursor] {
			return nil, &probeError{Code: "provider_protocol_error", Detail: "Invalid repeated discovery continuation."}
		}
		cursors[cursor] = true
		path = basePath + "?" + url.Values{parameter: []string{cursor}}.Encode()
	}
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
		if ValidModelName("model", m.ID) != nil || seen[m.ID] {
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

// certifyTuple uses a bounded live probe or authenticated native media discovery.
func (s *Server) certifyTuple(ctx context.Context, cfg *Configuration, credential []byte, model string, tuple CapabilityInput, maxEventBytes int) error {
	if !certifiable(cfg.Kind, value(cfg.Options.VendorID), tuple) {
		return &probeError{Code: "capability_unavailable", Detail: "This connector cannot certify the requested tuple."}
	}
	switch {
	case tuple.Operation == "batch":
		return s.certifyBatch(ctx, cfg, credential)
	case tuple.Operation == "realtime":
		return s.certifyRealtime(ctx, cfg, credential, model)
	case tuple.Surface == "bedrock":
		return s.certifyBedrockIngress(ctx, cfg, credential, model, tuple, maxEventBytes)
	}
	switch tuple.Operation {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
	default:
		return s.certifyNativeMedia(ctx, cfg, credential, model, tuple)
	}
	family := openai.FamilyChat
	payload := map[string]any{"model": "certification", "messages": []map[string]string{{"role": "user", "content": "Reply with OK."}}, "max_tokens": 16}
	switch tuple.Operation {
	case "token_count":
		family = openai.FamilyInputTokens
		payload = map[string]any{"model": "certification", "input": "Count these tokens."}
	case "embeddings":
		family = openai.FamilyEmbeddings
		payload = map[string]any{"model": "certification", "input": "embedding probe"}
	case "moderation":
		family = openai.FamilyModeration
		payload = map[string]any{"model": "certification", "input": "A friendly greeting."}
	case "rerank":
		family = openai.FamilyRerank
		payload = map[string]any{"model": "certification", "query": "rerank probe", "documents": []string{"first document", "second document"}}
	}
	if tuple.Surface == "anthropic" {
		family = openai.FamilyAnthropic
		if tuple.Operation == "token_count" {
			family = openai.FamilyAnthropicCount
			delete(payload, "input")
			payload["messages"] = []map[string]string{{"role": "user", "content": "Count these tokens."}}
			delete(payload, "max_tokens")
		}
	}
	if tuple.Surface == "gemini" {
		family = openai.FamilyGemini
		if tuple.Operation == "token_count" {
			family = openai.FamilyGeminiCount
		}
		payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []map[string]string{{"text": "Reply with OK."}}}}}
		if tuple.Operation == "generation" {
			payload["generationConfig"] = map[string]int{"maxOutputTokens": 16}
		}
	}
	if tuple.Mode == ModeStreaming {
		if tuple.Surface == "gemini" {
			family = openai.FamilyGeminiStream
		} else {
			payload["stream"] = true
		}
	}
	families := []openai.Family{family}
	if tuple.Operation == "generation" && tuple.Surface == "openai" && !protocols.ChatOnly(value(cfg.Options.VendorID)) && (cfg.Kind == KindOpenAI || cfg.Kind == KindAzure || cfg.Kind == KindOpenAICompatible) {
		families = append(families, openai.FamilyResponses)
	}
	for _, family := range families {
		current := payload
		if family == openai.FamilyResponses {
			current = map[string]any{"model": "certification", "input": "Reply with OK.", "max_output_tokens": 16, "stream": tuple.Mode == ModeStreaming}
		}
		data, _ := json.Marshal(current)
		parsed, err := protocols.Parse(family, data, "certification")
		if err != nil {
			return err
		}
		transport := cfg.transport()
		body, wire, err := protocols.Encode(parsed, cfg.Kind, value(cfg.Options.VendorID), transport.Model(model), cfg.Options.ParameterDefaults)
		if err != nil {
			return err
		}
		endpoint, err := transport.URL(wire, model, tuple.Mode == ModeStreaming)
		if err != nil {
			return err
		}
		status, data, err := s.call(ctx, cfg, credential, http.MethodPost, endpoint, body)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(status)
		}
		if tuple.Mode == ModeStreaming {
			_, err = protocols.Stream(wire, family, bytes.NewReader(data), maxEventBytes, "certification", true, func([]byte) error { return nil })
		} else {
			_, err = protocols.DecodeRequest(wire, family, data, "certification", protocols.EmbeddingEncoding(parsed, cfg.Options.ParameterDefaults), parsed)
		}
		if err != nil {
			return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not satisfy the requested native codec contract."}
		}
	}
	return nil
}

func (s *Server) certifyBatch(ctx context.Context, cfg *Configuration, credential []byte) error {
	prefix := ""
	query := "?limit=1"
	if cfg.Kind == KindAzure {
		prefix = "/openai"
		query = "?api-version=" + url.QueryEscape(value(cfg.APIVersion)) + "&limit=1"
	}
	for _, path := range []string{prefix + "/files" + query, prefix + "/batches" + query} {
		status, data, err := s.call(ctx, cfg, credential, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(status)
		}
		var list struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &list) != nil || list.Data == nil {
			return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not satisfy the batch listing contract."}
		}
	}
	return nil
}

func (s *Server) certifyRealtime(ctx context.Context, cfg *Configuration, credential []byte, model string) error {
	transport := cfg.transport()
	base := strings.TrimRight(transport.Endpoint, "/")
	query := url.Values{}
	var endpoint string
	if cfg.Kind == KindAzure {
		deployment := transport.Model(model)
		if deployment == "" {
			return &probeError{Code: "model_required", Detail: "Realtime certification requires a configured deployment."}
		}
		query.Set("api-version", value(cfg.APIVersion))
		query.Set("deployment", deployment)
		endpoint = base + "/openai/realtime?" + query.Encode()
	} else {
		upstream := transport.Model(model)
		if upstream == "" {
			return &probeError{Code: "model_required", Detail: "Realtime certification requires a configured model."}
		}
		query.Set("model", upstream)
		endpoint = base + "/realtime?" + query.Encode()
	}
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = "wss://" + strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		endpoint = "ws://" + strings.TrimPrefix(endpoint, "http://")
	default:
		return &probeError{Code: "invalid_endpoint", Detail: "The endpoint cannot serve realtime sessions."}
	}
	check := strings.Replace(endpoint, "wss://", "https://", 1)
	check = strings.Replace(check, "ws://", "http://", 1)
	u, err := url.Parse(check)
	if err != nil {
		return &probeError{Code: "invalid_endpoint", Detail: err.Error()}
	}
	u.RawQuery = ""
	if _, err := s.Egress.ValidateEndpoint(u.String()); err != nil {
		return &probeError{Code: "invalid_endpoint", Detail: err.Error()}
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
		return &probeError{Code: "invalid_endpoint", Detail: err.Error()}
	}
	if _, err := s.auth.Apply(ctx, req, transport, credential, nil); err != nil {
		return &probeError{Code: "credential_invalid", Detail: err.Error()}
	}
	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: s.client, HTTPHeader: req.Header})
	if err != nil {
		if resp != nil && resp.StatusCode != 0 {
			return statusError(resp.StatusCode)
		}
		return classify(err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

func (s *Server) certifyBedrockIngress(ctx context.Context, cfg *Configuration, credential []byte, model string, tuple CapabilityInput, maxEventBytes int) error {
	upstream := cfg.transport().Model(model)
	if upstream == "" {
		upstream = model
	}
	base := "/model/" + url.PathEscape(upstream) + "/"
	if tuple.Operation == "bedrock_invoke" {
		body, family := bedrockInvokeProbe(upstream)
		if family == "" {
			return &probeError{Code: "capability_unavailable", Detail: "InvokeModel ingress supports only qualified Anthropic Claude, Titan embeddings, and Titan image models."}
		}
		if tuple.Mode == ModeStreaming {
			if family != openai.FamilyAnthropic {
				return &probeError{Code: "capability_unavailable", Detail: "InvokeModelWithResponseStream ingress supports only qualified Anthropic Claude models."}
			}
			return s.bedrockProbe(ctx, cfg, credential, base+"invoke-with-response-stream", body, func(data []byte) error {
				return readBedrockProbeStream(data, maxEventBytes)
			})
		}
		return s.bedrockProbe(ctx, cfg, credential, base+"invoke", body, func(data []byte) error {
			if family == openai.FamilyAnthropic || family == openai.FamilyBedrockEmbeddings {
				if _, err := protocols.Decode(family, family, data, upstream, ""); err != nil {
					return err
				}
				return nil
			}
			return validateBedrockImageProbe(data)
		})
	}
	body, _ := json.Marshal(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "Reply with OK."}}}},
		"inferenceConfig": map[string]any{"maxTokens": 16},
	})
	if tuple.Mode == ModeStreaming {
		return s.bedrockProbe(ctx, cfg, credential, base+"converse-stream", body, func(data []byte) error {
			return readBedrockProbeStream(data, maxEventBytes)
		})
	}
	return s.bedrockProbe(ctx, cfg, credential, base+"converse", body, func(data []byte) error {
		_, err := protocols.Decode(openai.FamilyBedrock, openai.FamilyBedrock, data, upstream, "")
		return err
	})
}

func (s *Server) bedrockProbe(ctx context.Context, cfg *Configuration, credential []byte, path string, body []byte, validate func([]byte) error) error {
	status, data, err := s.call(ctx, cfg, credential, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(status)
	}
	if err := validate(data); err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not satisfy the requested native codec contract."}
	}
	return nil
}

func bedrockInvokeProbe(model string) ([]byte, openai.Family) {
	switch {
	case strings.Contains(model, "anthropic.claude"):
		return []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"Reply with OK."}]}]}`), openai.FamilyAnthropic
	case strings.HasPrefix(model, "amazon.titan-embed-text-"):
		return []byte(`{"inputText":"Reply with OK."}`), openai.FamilyBedrockEmbeddings
	case strings.HasPrefix(model, "amazon.titan-image-generator-"):
		return []byte(`{"taskType":"TEXT_IMAGE","textToImageParams":{"text":"A small blue square."},"imageGenerationConfig":{"numberOfImages":1,"width":512,"height":512}}`), openai.FamilyImageGeneration
	}
	return nil, ""
}

func validateBedrockImageProbe(data []byte) error {
	var wire struct {
		Images []string `json:"images"`
		Error  *string  `json:"error"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Error != nil && *wire.Error != "" || len(wire.Images) == 0 {
		return errors.New("invalid Titan image response")
	}
	return nil
}

func readBedrockProbeStream(data []byte, limit int) error {
	reader := bytes.NewReader(data)
	for {
		if _, err := protocols.ReadBedrockEvent(reader, limit); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// credentialFor prefers the enabled default slot, then another enabled slot.
func (s *Server) credentialFor(ctx context.Context, tx pgx.Tx, p *record) ([]byte, *credentialState, error) {
	slots, err := loadSlots(ctx, tx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	slot := selectProbeSlot(slots, &p.Configuration)
	if slot == nil {
		return nil, nil, access.Fail(422, "credential_required", "Enable a credential slot before probing this connection.")
	}
	state := &credentialState{ID: slot.CredentialID, Version: slot.CredentialVersion, Revoked: slot.CredentialRevoked}

	if !p.Configuration.CredentialRequired() {
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

func selectProbeSlot(slots []slotRow, cfg *Configuration) *slotRow {
	var best *slotRow
	usable := func(s *slotRow) bool {
		return !cfg.CredentialRequired() || s.CredentialID != nil && !s.CredentialRevoked
	}
	for i := range slots {
		row := &slots[i]
		if !row.Enabled {
			continue
		}
		if best == nil || usable(row) && !usable(best) || usable(row) == usable(best) && (row.Default && !best.Default || row.Default == best.Default && (row.Priority < best.Priority || row.Priority == best.Priority && row.Position < best.Position)) {
			best = row
		}
	}
	return best
}

// Media certification must not create billable images, audio or video jobs.
// Discovery proves access to the exact mapped model; the official OpenAI
// connector supplies the closed media wire contract. Custom hosts cannot use
// unrelated chat success or a self-reported model list as media evidence.
func (s *Server) certifyNativeMedia(ctx context.Context, cfg *Configuration, credential []byte, model string, tuple CapabilityInput) error {
	if tuple.Operation == "image_generation" && (cfg.Kind == KindVertex || cfg.Kind == KindBedrock) {
		return s.certifyNativeImage(ctx, cfg, credential, model)
	}
	endpoint, err := url.Parse(value(cfg.Endpoint))
	if err != nil || cfg.Kind != KindOpenAI || cfg.AuthMode != AuthAPIKey || len(credential) == 0 ||
		len(cfg.Options.CredentialHeaders) != 0 || endpoint.Scheme != "https" ||
		endpoint.Hostname() != "api.openai.com" || (endpoint.Port() != "" && endpoint.Port() != "443") ||
		strings.TrimRight(endpoint.EscapedPath(), "/") != "/v1" {
		return &probeError{Code: "capability_unavailable", Detail: "Media certification requires the official OpenAI endpoint and an API key."}
	}
	models, err := s.listModels(ctx, cfg, credential)
	if err != nil {
		return err
	}
	expected := cfg.transport().Model(model)
	if slices.Contains(models, expected) {
		return nil
	}
	return &probeError{Code: "model_unavailable", Detail: "The credential cannot discover the requested media model."}
}

func (s *Server) certifyNativeImage(ctx context.Context, cfg *Configuration, credential []byte, model string) error {
	count := int64(1)
	request := &media.Request{Op: media.OpImageGeneration, Route: "certification", Prompt: "A certification probe image.", Count: &count}
	call, failure := media.Encode(request, cfg.Kind, cfg.transport().Model(model))
	if failure != nil {
		return &probeError{Code: "capability_unavailable", Detail: failure.Message}
	}
	endpoint, err := cfg.transport().MediaURL(call.Path, model, call.Query)
	if err != nil || endpoint == "" {
		return &probeError{Code: "capability_unavailable", Detail: "The media probe endpoint could not be built."}
	}
	status, data, err := s.call(ctx, cfg, credential, call.Method, endpoint, call.JSON)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(status)
	}
	_, mErr := media.DecodeNativeImageResponse(call.Native, data, 1, func(b64 string, index int) (*media.Artifact, *media.Error) {
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return nil, media.Fail(502, "provider_protocol_error", "The provider image payload is not valid base64.")
		}
		return &media.Artifact{}, nil
	})
	if mErr != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream did not satisfy the requested native codec contract."}
	}
	return nil
}
