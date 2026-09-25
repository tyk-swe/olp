package providers

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/generation"
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
	if tuple.Operation == "generation" {
		// A profile-published registered generation dialect owns its
		// certifiability: the operation registry answers capability, not the
		// kind/vendor compatibility table.
		if _, ok := configuredGenerationDialect(cfg); ok {
			return cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
		}
	}
	return certifiable(cfg.Kind, value(cfg.Options.VendorID), tuple)
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
		return statusError(status)
	}
	if _, err := plan.Decode(body); err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The upstream violated its native operation result contract."}
	}
	return nil
}

// generationModelBinding reads the dialect's declared identity rules: a
// generation dialect whose grammar binds the serving model into a request
// member requires it; dialects that carry the model only in addressing, such
// as a models/ path, register none.
func generationModelBinding(d generation.Dialect) string {
	for _, rule := range d.IdentityRules {
		if rule.Pointer == "/model" && rule.Origin == oif.IdentityBinding {
			return string(operations.ModelRequired)
		}
	}
	return string(operations.ModelNone)
}

func (s *Server) operationDialects(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for _, d := range operationregistry.Default.Dialects() {
		// A nil schema emits null: the dialect registers no body schema, so
		// no document is invented for it.
		items = append(items, map[string]any{"id": d.Identity.ID, "revision": d.Identity.Revision, "operation": d.Operation.ID, "operation_revision": d.Operation.Revision, "surface": d.Surface, "mode": "unary", "label": d.Label, "model_binding": string(d.ModelBinding), "request_schema": d.RequestSchema, "result_schema": d.ResultSchema, "documentation": d.Documentation, "evidence": d.Evidence})
	}
	// Generation dialects are the streaming-capable generation contracts. They
	// own no request/result body schema — the wire grammar is the contract —
	// so both emit null rather than an invented document.
	for _, d := range operationregistry.Generation.Dialects() {
		modes := []string{"unary"}
		if d.Streaming {
			modes = append(modes, "streaming")
		}
		for _, mode := range modes {
			items = append(items, map[string]any{"id": d.Identity.ID, "revision": d.Identity.Revision, "operation": d.Operation.ID, "operation_revision": d.Operation.Revision, "surface": d.Surface, "mode": mode, "label": d.Label, "model_binding": generationModelBinding(d), "request_schema": nil, "result_schema": nil, "documentation": d.Documentation, "evidence": d.Evidence})
		}
	}
	return access.OK(map[string]any{"items": items}), nil
}
