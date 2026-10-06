package providers

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/vendors"
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
	if cfg.ProfileID == "gemini-live" {
		return tuple.Operation == "realtime" && tuple.Surface == "gemini" && tuple.Mode == "realtime" && cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
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
	endpoint, err := plan.Endpoint()
	if err != nil {
		return err
	}
	status, body, err := s.call(ctx, cfg, credential, http.MethodPost, endpoint, plan.Body())
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(cfg, status, body)
	}
	if _, err := plan.Decode(body); err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream violated its native operation result contract."}
	}
	return nil
}

func (s *Server) operationDialects(r *http.Request, _ access.Principal) (access.Reply, error) {
	items := []map[string]any{}
	for _, d := range operationregistry.Default.Dialects() {
		items = append(items, map[string]any{"id": d.Identity.ID, "revision": d.Identity.Revision, "operation": d.Operation.ID, "operation_revision": d.Operation.Revision, "surface": d.Surface, "mode": "unary", "label": d.Label, "request_schema": d.RequestSchema, "result_schema": d.ResultSchema, "documentation": d.Documentation, "evidence": d.Evidence})
	}
	return access.OK(map[string]any{"items": items}), nil
}
