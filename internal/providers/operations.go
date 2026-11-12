package providers

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
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

func configuredOperation(transport connectors.Config, operation string) (operations.Dialect, bool) {
	if transport.ProfileID == "" {
		return operations.Dialect{}, false
	}
	profile, err := transport.Profile()
	if err != nil {
		return operations.Dialect{}, false
	}
	codec, ok := operationregistry.Lookup(profile.OperationDialect(operation))
	return codec, ok && codec.Operation.ID == operation
}
func configurationCertifiable(transport connectors.Config, tuple connection.Capability) bool {
	if retainedapi.Operation(tuple.Operation) {
		return transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if transport.ProfileID == "gemini-live" {
		return tuple.Operation == "realtime" && tuple.Surface == "gemini" && tuple.Mode == "realtime" && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if duplexcontract.IsProfile(transport.ProfileID) {
		return tuple.Operation == "realtime" && tuple.Surface == "native" && tuple.Mode == "realtime" && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == "batch" {
		return transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if _, ok := configuredOperation(transport, tuple.Operation); ok {
		return transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == "generation" && tuple.Surface == "native" {
		// Only a profile in a native generation dialect serves this surface.
		return transport.ProfileID != "" && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	return certifiable(transport.Kind, transport.VendorID, tuple)
}

// defaultProbeTuple is the unary tuple a provider without model discovery
// certifies its declared models with. An explicit profile that cannot serve
// the default tuple, such as a dedicated embeddings or rerank dialect, is
// probed with the first unary tuple it can certify.
func defaultProbeTuple(transport connectors.Config) connection.Capability {
	if duplexcontract.IsProfile(transport.ProfileID) {
		if tuple := (connection.Capability{Operation: "realtime", Surface: "native", Mode: "realtime"}); probeable(transport, tuple) {
			return tuple
		}
	}
	operation := probeOperation(transport)
	if transport.Kind == connectors.KindVertexAI && transport.Hosting() != "vertex-anthropic" {
		operation = "token_count"
	}
	tuple := connection.Capability{Operation: operation, Surface: "openai", Mode: "unary"}
	if transport.ProfileID == "" || probeable(transport, tuple) {
		return tuple
	}
	profile, err := transport.Profile()
	if err != nil {
		return tuple
	}
	for _, op := range profile.Operations {
		for _, surface := range []string{"openai", "native", "anthropic", "gemini", "bedrock"} {
			if candidate := (connection.Capability{Operation: op, Surface: surface, Mode: "unary"}); probeable(transport, candidate) {
				return candidate
			}
		}
	}
	return tuple
}

// probeOperation is the operation that certifies a declared model of the
// configured vendor: the one its contract names, or generation.
func probeOperation(transport connectors.Config) string {
	if contract, ok := vendors.Lookup(transport.VendorID); ok && contract.ProbeOperation != "" {
		return contract.ProbeOperation
	}
	return "generation"
}

func probeable(transport connectors.Config, tuple connection.Capability) bool {
	return configurationCertifiable(transport, tuple) && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
}

func (s *Catalogue) certifyOperation(ctx context.Context, cfg probeTarget, credential []byte, model string, tuple connection.Capability, codec operations.Dialect) error {
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

func (s *Catalogue) pollOperation(ctx context.Context, cfg probeTarget, credential []byte, plan *operationplan.Plan, submit string, header http.Header) ([]byte, error) {
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

func (s *Catalogue) cancelOperationPoll(cfg probeTarget, credential []byte, plan *operationplan.Plan, location string) {
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

// Kind options include tuples available through an explicit profile, even
// when the kind's Automatic connector cannot serve them.
func capabilityOptionsForKind(kind string) []connection.Capability {
	var profiles []connectors.Config
	for _, profile := range connectors.Profiles() {
		if profile.Kind == kind {
			transport := connectors.Config{Kind: kind, ProfileID: profile.ID, ProfileRevision: profile.Revision, VendorID: profile.Vendor()}
			profiles = append(profiles, transport)
		}
	}
	out := []connection.Capability{}
	for _, tuple := range connection.Vocabulary() {
		profiled := slices.ContainsFunc(profiles, func(transport connectors.Config) bool {
			return probeable(transport, tuple)
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

func certifiable(kind, vendor string, c connection.Capability) bool {
	if c.Operation == "retrieval" && (kind == connectors.KindBedrock || kind == connectors.KindVertexAI || kind == connectors.KindAzureAISearch) {
		return false
	}
	if !connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode) {
		return false
	}
	switch c.Operation {
	case "batch":
		officialVendor := vendor == vendors.DefaultFor(kind)
		return c.Surface != "anthropic" && c.Surface != "gemini" || officialVendor
	case "generation", "token_count", "embeddings", "moderation", "rerank", "ocr", "search", "realtime", "bedrock_invoke":
		return true
	}
	if retainedapi.Operation(c.Operation) {
		return true
	}
	vendorMode := c.Mode == "unary" || c.Operation == "video_create" && c.Mode == "async"
	return kind == connectors.KindOpenAI || (reviewedMedia(kind, vendor, c.Operation) || mediaByCall(kind, vendor, c.Operation)) && vendorMode
}
