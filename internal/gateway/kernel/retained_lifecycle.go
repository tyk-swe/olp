package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

func (s *Server) registerRetainedLifecycle(mux *http.ServeMux) {
	for _, endpoint := range retainedapi.Endpoints() {
		if endpoint.Operation != "agent_resource" || !retainedapi.Supports("gemini", endpoint.Operation, endpoint.Surface, "unary") {
			continue
		}
		mux.HandleFunc(endpoint.Method+" "+endpoint.Pattern, func(w http.ResponseWriter, r *http.Request) { s.retainedLifecycle(w, r, endpoint) })
	}
}

func (s *Server) retainedSubmissionDigest(endpoint retainedapi.Endpoint, slug string, body []byte) ([]byte, *fault.Error) {
	digester := s.Deps.Digester
	if digester == nil {
		return nil, fault.ServerError(503, "authority_unavailable", "Resource submission identity is unavailable.")
	}
	canonical, err := CanonicalBatchJSON(body)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	return digester.Digest(secrets.RetainedSubmissionDigest, endpoint.Operation+"\x00"+endpoint.Kind+"\x00"+slug+"\x00"+string(canonical)), nil
}

func retainedAffinity(resource *resources.Resource, p *Pin, route *routing.Route) bool {
	return resource != nil && resource.RouteSlug == route.Slug && resource.RouteRevisionID == route.RevisionID && resource.ProviderID == p.Provider.ID && resource.ProviderRevisionID == p.Provider.RevisionID && resource.TargetID == p.Target.ID && resource.SlotID == p.Slot.ID && sameCredential(resource.CredentialID, p.Slot.CredentialID)
}
func sameCredential(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Server) retainedResponseLimit() int64 {
	if s.Config.MaxResponseBytes > 0 && s.Config.MaxResponseBytes < retainedBodyLimit {
		return s.Config.MaxResponseBytes
	}
	return retainedBodyLimit
}

func retainedAgentInput(source oif.Document) *fault.Error {
	if err := retainedAgentEnvironment(source); err != nil {
		return err
	}
	for _, field := range source.Root().Members() {
		if !slices.Contains([]string{"agent_config", "base_agent", "base_environment", "description", "system_instruction", "tools"}, field.Name) {
			return fault.InvalidRequest("unsupported_field", "The agent contract does not support field "+field.Name+".", nil)
		}
		if field.Value.Kind() == oif.Null {
			continue
		}
		expected := oif.String
		switch field.Name {
		case "agent_config", "base_environment":
			expected = oif.Object
		case "tools":
			expected = oif.Array
		}
		if field.Value.Kind() != expected {
			return fault.InvalidRequest("invalid_parameter", "Invalid value type for agent field: "+field.Name, nil)
		}
	}
	return nil
}

func prepareRetainedAgent(body []byte) (retainedapi.PreparedRewrite, error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
	if err != nil {
		return retainedapi.PreparedRewrite{}, err
	}
	if contractErr := retainedAgentEnvironment(source); contractErr != nil {
		return retainedapi.PreparedRewrite{}, resources.ErrContract
	}
	source, err = normalizeRetainedAgent(source)
	if err != nil {
		return retainedapi.PreparedRewrite{}, err
	}
	return retainedapi.PrepareRewrite(source, map[string]string{"/id": "agent_resource", "base_agent": "agent_resource"})
}

func (s *Server) projectRetainedAgent(ctx context.Context, resource *resources.Resource, rewrite retainedapi.PreparedRewrite) ([]byte, error) {
	projected, err := rewrite.Bind(func(kind, id string) (string, error) {
		if id == resource.UpstreamID {
			return resource.ID, nil
		}
		identity := resource.NativeIdentity()
		identity.Kind = kind
		identity.UpstreamID = id
		bound, findErr := s.Deps.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
		if findErr != nil {
			return "", retainedBindingStoreError(findErr)
		}
		return bound.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

// Provider errors may echo native resource identifiers. Preserve the stable
// HTTP status while keeping provider identities outside public errors.
func retainedPublicError(err *fault.Error, status int) *fault.Error {
	out := *err
	if status >= http.StatusBadRequest && status <= 599 {
		out.Status = status
	}
	out.Message = "The provider could not complete this resource operation."
	out.Param = nil
	out.Code = "provider_resource_error"
	out.Type = "provider_error"
	return &out
}

const retainedBodyLimit = 4 << 20

func encodeRetainedPage(response map[string]any) ([]byte, *fault.Error) {
	body, err := json.Marshal(response)
	if err != nil {
		return nil, fault.ServerError(500, "internal_error", "The resource list could not be encoded.")
	}
	if len(body) > retainedBodyLimit {
		return nil, fault.ServerError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page.")
	}
	return body, nil
}

func (s *Server) gateRetainedSlot(ctx context.Context, x *Call, p *Pin, route *routing.Route) *fault.Error {
	if p.Hold != nil {
		return nil
	}
	// Bound work crosses the same spend-cap boundary as a fresh attempt:
	// gateSlot alone would reserve token quota but never the route,
	// provider, or credential-slot spend caps pinned on the attempt.
	// Spend caps are evaluated against the attempt's route, which some
	// collection handlers carry only as this argument.
	if x.Route == nil {
		x.Route = route
	}
	provider, slot := &p.Provider, &p.Slot
	if x.freeRetainedControl() {
		copyProvider := p.Provider
		copySlot := p.Slot
		copySlot.TokensPerMinute = nil
		copySlot.InputTokensPerMinute = nil
		copySlot.OutputTokensPerMinute = nil
		if p.Provider.Limits != nil {
			copyLimits := *p.Provider.Limits
			copyLimits.TokensPerMinute = nil
			copyLimits.InputTokensPerMinute = nil
			copyLimits.OutputTokensPerMinute = nil
			copyProvider.Limits = &copyLimits
		}
		provider, slot = &copyProvider, &copySlot
	}
	gate := s.gateAttempt(ctx, x, p.attempt, provider, slot, quota.CombinedDemand(max(resourceEstimate, x.estimate)), s.Now().Add(time.Duration(route.OverallTimeout)*time.Millisecond))
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.Hold = gate.hold
	return nil
}

// Environment and credential references require their own owner-scoped
// contracts. Inline environment values may pass without provider references.
func retainedAgentEnvironment(source oif.Document) *fault.Error {
	v, present := source.Root().Lookup("base_environment")
	if present && v.Kind() != oif.Null && v.Kind() != oif.Object {
		return PolicyUnavailable("unsupported_stateful_reference", "Native environment references are not supported by this agent resource contract.")
	}
	var bindings []oif.Value
	if env, ok := v.Lookup("env"); ok {
		for _, variable := range env.Members() {
			bindings = append(bindings, variable.Value)
		}
	}
	if network, ok := v.Lookup("network"); ok {
		if allowlist, ok := network.Lookup("allowlist"); ok {
			bindings = append(bindings, allowlist.Elements()...)
		}
	}
	for _, binding := range bindings {
		if credential, ok := binding.Lookup("credential"); ok && credential.Kind() != oif.Null {
			return PolicyUnavailable("unsupported_stateful_reference", "Native credential references are not supported by this agent resource contract.")
		}
	}
	return nil
}

// The native SDK represents a standalone agent with an empty optional base.
func normalizeRetainedAgent(source oif.Document) (oif.Document, error) {
	if value, present := source.Root().Lookup("base_agent"); present {
		if id, text := value.Text(); text && id == "" {
			return oif.Apply(source, []oif.Change{{Pointer: "/base_agent", Remove: true, Origin: oif.ResourceBinding, Reason: "empty optional base agent"}})
		}
	}
	return source, nil
}

// Retained controls require durable credential identity and known, unpriced
// execution because this contract has no model usage observations.
func (s *Server) retainedControlPolicy(x *Call, p *Pin, route *routing.Route) *fault.Error {
	if p.Provider.CredentialSource == "caller" {
		return PolicyUnavailable("unsupported_provider_credentials", "Agent resources require managed provider credentials.")
	}
	if x.routingInputs == nil && s.Deps.Runtime != nil {
		x.routingInputs = s.Deps.Runtime.RoutingInputs()
	}
	if x.routingInputs == nil || x.request.startedAt.Sub(x.routingInputs.RefreshedAt) > routinginputs.PerformanceMaxAge {
		return fault.ServerError(http.StatusServiceUnavailable, "pricing_unavailable", "Current resource pricing is unavailable; retry shortly.")
	}
	if p.attempt.Price == nil {
		p.attempt.Price = x.routingInputs.Price(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, p.Model, x.operationName(), x.request.startedAt)
	}
	if p.attempt.Price != nil || x.attemptCardRevisionForRoute(p.Provider.ID, route, x.request.startedAt) != nil {
		return PolicyUnavailable("unsupported_pricing", "Agent resource control does not support priced execution. Use an unpriced resource route.")
	}
	return nil
}

func retainedUnsettled(state string) bool {
	return state == "submitting" || state == "submission_unknown" || state == "not_accepted"
}
