package providers

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/vendors"
)

const (
	maxProbePolls   = 40
	probePollWait   = 250 * time.Millisecond
	minProbePoll    = 50 * time.Millisecond
	maxProbePoll    = 5 * time.Second
	probeCancelWait = 5 * time.Second
)

func configuredOperation(cfg *connection.Configuration, operation string) (operations.Dialect, bool) {
	if cfg.ProfileID == "" {
		return operations.Dialect{}, false
	}
	profile, err := cfg.Transport().Profile()
	if err != nil {
		return operations.Dialect{}, false
	}
	codec, ok := operationregistry.Lookup(profile.OperationDialect(operation))
	return codec, ok && codec.Operation.ID == operation
}
func configurationCertifiable(cfg *connection.Configuration, tuple CapabilityInput) bool {
	if retainedapi.Operation(tuple.Operation) {
		return cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if cfg.ProfileID == "gemini-live" {
		return tuple.Operation == "realtime" && tuple.Surface == "gemini" && tuple.Mode == "realtime" && cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if duplexcontract.IsProfile(cfg.ProfileID) {
		return tuple.Operation == "realtime" && tuple.Surface == "native" && tuple.Mode == "realtime" && cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == "batch" {
		return cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if _, ok := configuredOperation(cfg, tuple.Operation); ok {
		return cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == OperationGeneration && tuple.Surface == "native" {
		// Only a profile in a native generation dialect serves this surface.
		return cfg.ProfileID != "" && cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	return certifiable(cfg.Kind, providerconfig.Value(cfg.Options.VendorID), tuple)
}

// defaultProbeTuple is the unary tuple a provider without model discovery
// certifies its declared models with. An explicit profile that cannot serve
// the default tuple, such as a dedicated embeddings or rerank dialect, is
// probed with the first unary tuple it can certify.
func defaultProbeTuple(cfg *connection.Configuration) CapabilityInput {
	if duplexcontract.IsProfile(cfg.ProfileID) {
		if tuple := (CapabilityInput{Operation: "realtime", Surface: "native", Mode: "realtime"}); probeable(cfg, tuple) {
			return tuple
		}
	}
	operation := probeOperation(cfg)
	if cfg.Kind == connectors.KindVertexAI && cfg.Transport().Hosting() != "vertex-anthropic" {
		operation = "token_count"
	}
	tuple := CapabilityInput{Operation: operation, Surface: "openai", Mode: ModeUnary}
	if cfg.ProfileID == "" || probeable(cfg, tuple) {
		return tuple
	}
	profile, err := cfg.Transport().Profile()
	if err != nil {
		return tuple
	}
	for _, op := range profile.Operations {
		for _, surface := range []string{"openai", "native", "anthropic", "gemini", "bedrock"} {
			if candidate := (CapabilityInput{Operation: op, Surface: surface, Mode: ModeUnary}); probeable(cfg, candidate) {
				return candidate
			}
		}
	}
	return tuple
}

// probeOperation is the operation that certifies a declared model of the
// configured vendor: the one its contract names, or generation.
func probeOperation(cfg *connection.Configuration) string {
	if contract, ok := vendors.Lookup(providerconfig.Value(cfg.Options.VendorID)); ok && contract.ProbeOperation != "" {
		return contract.ProbeOperation
	}
	return "generation"
}

func probeable(cfg *connection.Configuration, tuple CapabilityInput) bool {
	return configurationCertifiable(cfg, tuple) && cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
}

func (s *Catalogue) certifyOperation(ctx context.Context, cfg *connection.Configuration, credential []byte, model string, tuple CapabilityInput, codec operations.Dialect) error {
	template, err := operationplan.Compile(operationplan.Config{Provider: cfg.Transport(), Model: model, Operation: tuple.Operation})
	if err != nil {
		return &probeError{Code: "capability_unavailable", Detail: "The native operation contract could not be compiled."}
	}
	// Certification invokes the target's native probe and validates its full
	// response. It does not certify every cross-dialect request or client contract.
	source, err := operationplan.Parse(codec.Identity.ID, codec.Probe("certification"), 1<<20)
	if err != nil {
		return &probeError{Code: "capability_unavailable", Detail: "The native operation probe is invalid."}
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "certification", ClientContract: operations.RawVectorClient})
	if err != nil {
		return &probeError{Code: "capability_unavailable", Detail: "The native operation probe cannot bind its configured defaults."}
	}
	submit, err := plan.RequestURL()
	if err != nil {
		return &probeError{Code: "capability_unavailable", Detail: "The native operation probe could not be addressed."}
	}
	status, header, raw, err := s.callWithHeaders(ctx, cfg, credential, plan.Method(), submit, plan.WireBody())
	if err != nil {
		return err
	}
	if plan.Transport() == operations.TransportPoll202 && status == http.StatusAccepted {
		raw, err = s.pollOperation(ctx, cfg, credential, plan, submit, header)
		if err != nil {
			return err
		}
	} else if status != http.StatusOK {
		return statusError(cfg, status, raw)
	}
	if _, err := plan.Decode(raw); err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream violated its native operation result contract."}
	}
	return nil
}

func (s *Catalogue) pollOperation(ctx context.Context, cfg *connection.Configuration, credential []byte, plan *operationplan.Plan, submit string, header http.Header) ([]byte, error) {
	location, err := plan.PollLocation(submit, header)
	if err != nil {
		return nil, &probeError{Code: "provider_protocol_error", Detail: "The upstream polling location is invalid."}
	}
	waitHeader := header
	for polls := 0; polls < maxProbePolls; polls++ {
		if polls > 0 {
			wait := probePollWait
			if waitHeader.Get("Retry-After") != "" {
				wait = min(max(upstream.RetryAfter(waitHeader.Get("Retry-After"), time.Now()), minProbePoll), maxProbePoll)
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				s.cancelOperationPoll(cfg, credential, plan, location)
				return nil, classify(ctx.Err())
			}
		}
		status, pollHeader, raw, err := s.callWithHeaders(ctx, cfg, credential, http.MethodGet, location, nil)
		if err != nil {
			if ctx.Err() != nil {
				s.cancelOperationPoll(cfg, credential, plan, location)
			}
			return nil, err
		}
		waitHeader = pollHeader
		switch {
		case status == http.StatusOK:
			state, stateErr := plan.PollState(raw)
			if stateErr != nil {
				return nil, &probeError{Code: "provider_protocol_error", Detail: "The upstream violated its native operation result contract."}
			}
			if state == operations.PollSucceeded {
				return raw, nil
			}
			if state == operations.PollFailed {
				return nil, &probeError{Code: "upstream_rejected", Detail: "The upstream operation failed."}
			}
		case status == http.StatusTooManyRequests || status >= 500:
			continue
		default:
			return nil, statusError(cfg, status, raw)
		}
	}
	s.cancelOperationPoll(cfg, credential, plan, location)
	return nil, &probeError{Code: "gateway_timeout", Detail: "The upstream operation did not finish within the probe deadline."}
}

func (s *Catalogue) cancelOperationPoll(cfg *connection.Configuration, credential []byte, plan *operationplan.Plan, location string) {
	method, target, ok := plan.CancelPoll(location)
	if !ok || method == "" || target == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeCancelWait)
	defer cancel()
	_, _, _ = s.call(ctx, cfg, credential, method, target, nil)
}

func (s *Server) operationDialects(r *http.Request, _ access.Principal) (access.Reply, error) {
	items := []map[string]any{}
	for _, d := range operationregistry.Catalog().Dialects() {
		items = append(items, map[string]any{"id": d.Identity.ID, "revision": d.Identity.Revision, "operation": d.Operation.ID, "operation_revision": d.Operation.Revision, "surface": d.Surface, "mode": "unary", "label": d.Label, "request_schema": d.RequestSchema, "result_schema": d.ResultSchema, "documentation": d.Documentation, "evidence": d.Evidence})
	}
	return access.OK(map[string]any{"items": items}), nil
}

// Capability tuple vocabulary shared across providers.
const (
	OperationGeneration = "generation"
	SurfaceOpenAI       = "openai"
	ModeUnary           = "unary"
	ModeStreaming       = "streaming"
)

type CapabilityInput struct {
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
}

var capabilityVocabulary = capabilityOptions()

func capabilityOptions() []CapabilityInput {
	tuple := func(operation, surface, mode string) CapabilityInput {
		return CapabilityInput{Operation: operation, Surface: surface, Mode: mode}
	}
	out := []CapabilityInput{tuple(OperationGeneration, SurfaceOpenAI, ModeUnary), tuple(OperationGeneration, SurfaceOpenAI, ModeStreaming)}
	for _, surface := range []string{"openai", "anthropic", "gemini"} {
		if surface != "openai" {
			out = append(out, tuple("generation", surface, "unary"), tuple("generation", surface, "streaming"))
		}
		out = append(out, tuple("token_count", surface, "unary"))
	}
	out = append(out, tuple("batch", "gemini", "unary"))
	for _, operation := range []string{"embeddings", "moderation", "rerank", "translation", "decision"} {
		out = append(out, tuple(operation, "openai", "unary"))
	}
	for _, operation := range []string{"ocr", "search"} {
		out = append(out, tuple(operation, "openai", "unary"), tuple(operation, "native", "unary"))
	}
	for _, operation := range []string{"image_generation", "image_edit", "speech", "transcription"} {
		out = append(out, tuple(operation, "openai", "unary"), tuple(operation, "openai", "streaming"))
	}
	for _, operation := range []string{"image_variation", "video_list", "video_get", "video_content", "video_delete"} {
		out = append(out, tuple(operation, "openai", "unary"))
	}
	out = append(out, tuple("video_create", "openai", "async"), tuple("batch", "openai", "unary"), tuple("batch", "anthropic", "unary"), tuple("realtime", "openai", "realtime"), tuple("realtime", "openai", "webrtc"), tuple("realtime", "gemini", "realtime"), tuple("realtime", "native", "realtime"))
	for _, operation := range []string{"generation", "bedrock_invoke"} {
		out = append(out, tuple(operation, "bedrock", "unary"), tuple(operation, "bedrock", "streaming"))
	}
	// Native generation dialects, which only their profiles serve.
	out = append(out, tuple("generation", "native", "unary"), tuple("generation", "native", "streaming"), tuple("batch", "native", "unary"))
	out = append(out, tuple("decision", "native", "unary"))
	out = append(out, tuple("agent_resource", "gemini", "unary"))
	for _, operation := range []string{"vector_store", "conversation", "container", "eval", "retrieval"} {
		out = append(out, tuple(operation, "openai", "unary"))
	}
	out = append(out, tuple("fine_tuning", "openai", "unary"), tuple("fine_tuning", "native", "unary"),
		tuple("skill", "anthropic", "unary"), tuple("context_cache", "gemini", "unary"))
	return out
}

// Kind options include tuples available through an explicit profile, even
// when the kind's Automatic connector cannot serve them.
func capabilityOptionsForKind(kind string) []CapabilityInput {
	var profiles []connection.Configuration
	for _, profile := range connectors.Profiles() {
		if profile.Kind == kind {
			cfg := connection.Configuration{Identity: providerconfig.Identity[*string]{Kind: kind, ProfileID: profile.ID, ProfileRevision: profile.Revision}}
			if kind == connectors.KindOpenAICompatible && strings.HasSuffix(profile.ID, "-files-batches") {
				vendor := strings.TrimSuffix(profile.ID, "-files-batches")
				cfg.Options.VendorID = &vendor
			}
			profiles = append(profiles, cfg)
		}
	}
	out := []CapabilityInput{}
	for _, tuple := range capabilityVocabulary {
		profiled := slices.ContainsFunc(profiles, func(cfg connection.Configuration) bool {
			return configurationCertifiable(&cfg, tuple) && cfg.Transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
		})
		if certifiable(kind, vendors.DefaultFor(kind), tuple) || profiled {
			out = append(out, tuple)
		}
	}
	return out
}

// Custom endpoints need a safe live probe; native media instead relies on the
// official connector contract and authenticated discovery.
// reviewedMedia reports a reviewed vendor's media operation through its own
// connector, which certifies costlessly: by its model listing or its account
// probe, beside its codec contract. No reviewed vendor streams media.
func reviewedMedia(kind, vendor, operation string) bool {
	contract, ok := vendors.Lookup(vendor)
	if !ok || contract.Connector != kind || !contract.Serves(operation) {
		return false
	}
	return contract.AccountProbe != "" || contract.Discovery && vendors.MediaWire(vendor, operation) != ""
}

func certifiable(kind, vendor string, c CapabilityInput) bool {
	if c.Operation == "retrieval" && (kind == connectors.KindBedrock || kind == connectors.KindVertexAI || kind == connectors.KindAzureAISearch) {
		return false
	} // Cloud retrieval requires its dedicated profile.
	if retainedapi.Operation(c.Operation) {
		return connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode)
	}
	if !connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode) {
		return false
	}
	switch c.Operation {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
		return true
	case "ocr", "search":
		return true
	case "batch":
		if c.Surface == "native" {
			return kind == connectors.KindVertexAI && vendor == "google-vertex" || kind == connectors.KindBedrock && vendor == "amazon-bedrock"
		}
		if c.Surface == "anthropic" {
			return kind == connectors.KindAnthropic && vendor == "anthropic"
		}
		if c.Surface == "gemini" {
			return kind == connectors.KindGemini && vendor == "google"
		}
		if c.Surface == "openai" && kind == connectors.KindGemini {
			return vendor == "google"
		}
		return kind == connectors.KindOpenAI || kind == connectors.KindAzureOpenAI || kind == connectors.KindOpenAICompatible &&
			(vendor == "groq" || vendor == "together" || vendor == "mistral")
	case "realtime":
		if c.Mode == "webrtc" {
			return kind == connectors.KindOpenAI
		}
		if c.Surface == "native" {
			return kind == connectors.KindOpenAICompatible && vendor == "xai" ||
				kind == connectors.KindVertexAI && vendor == "google-vertex" ||
				kind == connectors.KindBedrock && vendor == "amazon-bedrock"
		}
		return kind == connectors.KindOpenAI || kind == connectors.KindAzureOpenAI
	case "bedrock_invoke":
		return kind == connectors.KindBedrock
	default:
		// Reviewed vendor media is unary, but for the job a video creation
		// starts.
		vendorMode := c.Mode == ModeUnary || c.Operation == "video_create" && c.Mode == "async"
		return kind == connectors.KindOpenAI || (reviewedMedia(kind, vendor, c.Operation) || mediaByCall(kind, vendor, c.Operation)) && vendorMode
	}
}
