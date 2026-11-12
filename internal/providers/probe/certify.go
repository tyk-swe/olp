package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/vendors"
)

func (p *Prober) Certify(ctx context.Context, t Target, model string, tuple connection.Capability, maxEventBytes int) error {
	if !ConfigurationCertifiable(t.Connection.Transport(), tuple) || !t.Connection.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode) {
		return &Failure{Reason: CapabilityUnavailable, Detail: "This connector cannot certify the requested tuple."}
	}
	if retainedapi.Operation(tuple.Operation) {
		return p.certifyRetained(ctx, t, model, tuple)
	}
	if t.Connection.Document().ProfileID == "gemini-interactions" && tuple.Operation == "generation" && tuple.Surface == "gemini" {
		return p.certifyGeminiInteraction(ctx, t, model, tuple.Mode, maxEventBytes)
	}
	if t.Connection.Document().ProfileID == "gemini-live" && tuple.Operation == "realtime" && tuple.Surface == "gemini" {
		return p.certifyGeminiLive(ctx, t, model, maxEventBytes)
	}
	if duplexcontract.IsProfile(t.Connection.Document().ProfileID) && tuple.Operation == "realtime" && tuple.Surface == "native" {
		return p.certifyNativeDuplex(ctx, t, model)
	}
	if codec, ok := configuredOperation(t.Connection.Transport(), tuple.Operation); ok {
		return p.certifyOperation(ctx, t, model, tuple, codec)
	}
	switch {
	case tuple.Operation == "batch":
		return p.certifyBatch(ctx, t)
	case tuple.Operation == "realtime" && tuple.Mode == "webrtc":
		return p.certifyWebRTC(ctx, t, model)
	case tuple.Operation == "realtime":
		return p.certifyRealtime(ctx, t, model)
	case tuple.Surface == "bedrock":
		return p.certifyBedrockIngress(ctx, t, model, tuple, maxEventBytes)
	}
	switch tuple.Operation {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
	default:
		return p.certifyNativeMedia(ctx, t, model, tuple)
	}
	family := wire.Chat
	payload := map[string]any{"model": "certification", "messages": []map[string]string{{"role": "user", "content": "Reply with OK."}}, "max_tokens": 16}
	switch tuple.Operation {
	case "token_count":
		family = wire.InputTokens
		payload = map[string]any{"model": "certification", "input": "Count these tokens."}
	case "embeddings":
		family = wire.Embeddings
		payload = map[string]any{"model": "certification", "input": "embedding probe"}
	case "moderation":
		family = wire.Moderation
		payload = map[string]any{"model": "certification", "input": "A friendly greeting."}
	case "rerank":
		family = wire.Rerank
		payload = map[string]any{"model": "certification", "query": "rerank probe", "documents": []string{"first document", "second document"}}
	}
	if tuple.Surface == "anthropic" {
		family = wire.Anthropic
		if tuple.Operation == "token_count" {
			family = wire.AnthropicCount
			delete(payload, "input")
			payload["messages"] = []map[string]string{{"role": "user", "content": "Count these tokens."}}
			delete(payload, "max_tokens")
		}
	}
	if tuple.Surface == "gemini" {
		family = wire.Gemini
		if tuple.Operation == "token_count" {
			family = wire.GeminiCount
		}
		payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []map[string]string{{"text": "Reply with OK."}}}}}
		if tuple.Operation == "generation" {
			payload["generationConfig"] = map[string]int{"maxOutputTokens": 16}
		}
	}
	if tuple.Surface == "native" {
		// A native generation dialect is probed in its own grammar.
		family, _ = t.Connection.Transport().TargetFamily(wire.Chat)
		payload = protocols.NativeGenerationProbe(family)
	}
	if tuple.Mode == "streaming" {
		if tuple.Surface == "gemini" {
			family = wire.GeminiStream
		} else {
			payload["stream"] = true
		}
	}
	families := []wire.Format{family}
	if t.Connection.Document().ProfileID != "" && tuple.Operation == "generation" && tuple.Surface == "openai" {
		profile, _ := t.Connection.Transport().Profile()
		if profile.Dialect == "openai-completions" {
			families = []wire.Format{wire.Completions}
		} else if profile.Dialect == "openai-responses" {
			families = []wire.Format{wire.Responses}
		}
	}
	if t.Connection.Document().ProfileID == "" && tuple.Operation == "generation" && tuple.Surface == "openai" && vendors.Speaks(providerconfig.Value(t.Connection.Document().Options.VendorID), "openai-responses") && (t.Connection.Document().Kind == connectors.KindOpenAI || t.Connection.Document().Kind == connectors.KindAzureOpenAI || t.Connection.Document().Kind == connectors.KindOpenAICompatible) {
		families = append(families, wire.Responses)
	}
	for _, family := range families {
		current := payload
		if family == wire.Completions {
			current = map[string]any{"model": "certification", "prompt": "Reply with OK.", "max_tokens": 16, "stream": tuple.Mode == "streaming"}
		} else if family == wire.Responses {
			current = map[string]any{"model": "certification", "input": "Reply with OK.", "max_output_tokens": 16, "stream": tuple.Mode == "streaming"}
		}
		data, _ := json.Marshal(current)
		parsed, err := protocols.Parse(family, data, "certification")
		if err != nil {
			return err
		}
		transport := t.Connection.Transport()
		invocation, err := providerinvoke.Prepare(parsed, transport, model, t.Connection.Document().Options.ParameterDefaults)
		if err != nil {
			return err
		}
		body, encodedWire := invocation.Prepared.Document().Bytes(), invocation.Wire
		// A profile that forces streaming serves a non-streaming tuple through
		// the stream the gateway aggregates.
		aggregated := tuple.Mode != "streaming" && transport.ForcesStreaming()
		endpoint, err := transport.URL(encodedWire, model, tuple.Mode == "streaming" || aggregated)
		if err != nil {
			return err
		}
		status, data, err := p.call(ctx, t, http.MethodPost, endpoint, transport.WrapRequest(body, model))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(t, status, data)
		}
		switch {
		case tuple.Mode == "streaming":
			_, err = protocols.Stream(encodedWire, family, transport.StreamPayload(bytes.NewReader(data), maxEventBytes), maxEventBytes, "certification", true, func([]byte) error { return nil })
		case aggregated:
			var streamed *wire.Completion
			if streamed, err = openai.Aggregate(encodedWire, transport.StreamPayload(bytes.NewReader(data), maxEventBytes), maxEventBytes, BodyLimit); err == nil {
				_, err = protocols.DecodeRequest(encodedWire, family, streamed.Body, "certification", "", parsed)
			}
		default:
			_, err = protocols.DecodeRequest(encodedWire, family, transport.UnwrapResponse(data), "certification", protocols.EmbeddingEncoding(parsed, t.Connection.Document().Options.ParameterDefaults), parsed)
		}
		if err != nil {
			return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not satisfy the requested native codec contract."}
		}
	}
	return nil
}

func (p *Prober) certifyBatch(ctx context.Context, t Target) error {
	if t.Connection.Document().ProfileID == "vertex-batch-prediction" {
		endpoint, err := t.Connection.Transport().VertexBatchJobsURL("", "")
		if err != nil {
			return err
		}
		status, body, err := p.call(ctx, t, http.MethodGet, endpoint+"?pageSize=1", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(t, status, body)
		}
		if !validBatchListing(body, "batchPredictionJobs", true) {
			return &Failure{Reason: ProviderProtocolError, Detail: "Vertex returned a malformed batch job list."}
		}
		return nil
	}
	if t.Connection.Document().ProfileID == "bedrock-batch-inference" {
		endpoint, err := t.Connection.Transport().BedrockBatchJobsURL("", "")
		if err != nil {
			return err
		}
		status, body, err := p.call(ctx, t, http.MethodGet, endpoint+"?maxResults=1", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(t, status, body)
		}
		var listing struct {
			Jobs []json.RawMessage `json:"invocationJobSummaries"`
		}
		if json.Unmarshal(body, &listing) != nil || listing.Jobs == nil {
			return &Failure{Reason: ProviderProtocolError, Detail: "Bedrock returned a malformed batch job list."}
		}
		return nil
	}
	type listingProbe struct {
		resource, array string
		query           url.Values
		header          http.Header
		optional        bool
	}
	probes := []listingProbe{
		{resource: "files", array: "data", query: url.Values{"limit": {"1"}}},
		{resource: "batches", array: "data", query: url.Values{"limit": {"1"}}},
	}
	adapter, _ := t.Connection.Transport().FilesBatchesAdapter()
	switch {
	case t.Connection.Document().Kind == connectors.KindAnthropic:
		probes[0].query.Set("beta", "true")
		probes[0].header = anthropicFilesBeta()
		probes[1].resource = "messages/batches"
	case t.Connection.Document().Kind == connectors.KindGemini:
		probes = []listingProbe{
			{resource: "files", array: "files", query: url.Values{"pageSize": {"1"}}, optional: true},
			{resource: "batches", array: "operations", query: url.Values{"pageSize": {"1"}}, optional: true},
		}
	case adapter == connectors.BatchMistral:
		probes[0].query = nil
		probes[1].resource, probes[1].query = "batch/jobs", url.Values{"page_size": {"1"}}
	case adapter == connectors.BatchTogether:
		probes = []listingProbe{{resource: "batches"}}
	}
	for _, probe := range probes {
		path, err := t.Connection.Transport().ResourceURL("", probe.resource, probe.query)
		if err != nil {
			return err
		}
		resp, err := p.do(ctx, t, request{method: http.MethodGet, path: path, header: probe.header})
		if err != nil {
			return err
		}
		status, data := resp.status, resp.body
		if status != http.StatusOK {
			return statusError(t, status, data)
		}
		if !validBatchListing(data, probe.array, probe.optional) {
			return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not satisfy the batch listing contract."}
		}
	}
	return nil
}

func validBatchListing(data []byte, field string, optional bool) bool {
	array := json.RawMessage(data)
	if field != "" {
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil || object == nil {
			return false
		}
		var present bool
		array, present = object[field]
		if !present && optional {
			return true // Protobuf JSON omits empty repeated fields.
		}
	}
	var items []json.RawMessage
	return json.Unmarshal(array, &items) == nil && items != nil
}

func (p *Prober) certifyListedModel(ctx context.Context, t Target, model string) error {
	models, err := p.ListModels(ctx, t)
	if err != nil {
		return err
	}
	if slices.Contains(models, t.Connection.Transport().Model(model)) {
		return nil
	}
	return &Failure{Reason: ModelUnavailable, Detail: "The credential cannot discover the requested media model."}
}
