package providers

import (
	"context"
	"net/http"
	"time"

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

func configuredOperation(cfg *Configuration, operation string) (operations.Dialect, bool) {
	if cfg.ProfileID == "" {
		return operations.Dialect{}, false
	}
	profile, err := cfg.transport().Profile()
	if err != nil {
		return operations.Dialect{}, false
	}
	codec, ok := operationregistry.Lookup(profile.OperationDialect(operation))
	return codec, ok && codec.Operation.ID == operation
}
func configurationCertifiable(cfg *Configuration, tuple CapabilityInput) bool {
	if retainedapi.Operation(tuple.Operation) {
		return cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if cfg.ProfileID == "gemini-live" {
		return tuple.Operation == "realtime" && tuple.Surface == "gemini" && tuple.Mode == "realtime" && cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if duplexcontract.IsProfile(cfg.ProfileID) {
		return tuple.Operation == "realtime" && tuple.Surface == "native" && tuple.Mode == "realtime" && cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == "batch" {
		return cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if _, ok := configuredOperation(cfg, tuple.Operation); ok {
		return cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == OperationGeneration && tuple.Surface == "native" {
		// Only a profile in a native generation dialect serves this surface.
		return cfg.ProfileID != "" && cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	return certifiable(cfg.Kind, value(cfg.Options.VendorID), tuple)
}

// defaultProbeTuple is the unary tuple a provider without model discovery
// certifies its declared models with. An explicit profile that cannot serve
// the default tuple, such as a dedicated embeddings or rerank dialect, is
// probed with the first unary tuple it can certify.
func defaultProbeTuple(cfg *Configuration) CapabilityInput {
	operation := probeOperation(cfg)
	if cfg.Kind == KindVertex && cfg.transport().Hosting() != "vertex-anthropic" {
		operation = "token_count"
	}
	tuple := CapabilityInput{Operation: operation, Surface: "openai", Mode: ModeUnary}
	if cfg.ProfileID == "" || probeable(cfg, tuple) {
		return tuple
	}
	profile, err := cfg.transport().Profile()
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
func probeOperation(cfg *Configuration) string {
	if contract, ok := vendors.Lookup(value(cfg.Options.VendorID)); ok && contract.ProbeOperation != "" {
		return contract.ProbeOperation
	}
	return "generation"
}

func probeable(cfg *Configuration, tuple CapabilityInput) bool {
	return configurationCertifiable(cfg, tuple) && cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
}

func (s *Server) certifyOperation(ctx context.Context, cfg *Configuration, credential []byte, model string, tuple CapabilityInput, codec operations.Dialect) error {
	template, err := operationplan.Compile(operationplan.Config{Provider: cfg.transport(), Model: model, Operation: tuple.Operation})
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

func (s *Server) pollOperation(ctx context.Context, cfg *Configuration, credential []byte, plan *operationplan.Plan, submit string, header http.Header) ([]byte, error) {
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

func (s *Server) cancelOperationPoll(cfg *Configuration, credential []byte, plan *operationplan.Plan, location string) {
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
	for _, d := range operationregistry.Default.Dialects() {
		items = append(items, map[string]any{"id": d.Identity.ID, "revision": d.Identity.Revision, "operation": d.Operation.ID, "operation_revision": d.Operation.Revision, "surface": d.Surface, "mode": "unary", "label": d.Label, "request_schema": d.RequestSchema, "result_schema": d.ResultSchema, "documentation": d.Documentation, "evidence": d.Evidence})
	}
	return access.OK(map[string]any{"items": items}), nil
}
