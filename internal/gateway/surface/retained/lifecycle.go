package retained

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func (s surface) registerRetainedLifecycle(mux *http.ServeMux) {
	for _, endpoint := range retainedapi.Endpoints() {
		if endpoint.Operation != "agent_resource" || !retainedapi.Supports("gemini", endpoint.Operation, endpoint.Surface, "unary") {
			continue
		}
		mux.HandleFunc(endpoint.Method+" "+endpoint.Pattern, func(w http.ResponseWriter, r *http.Request) { s.retainedLifecycle(w, r, endpoint) })
	}
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
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: kernel.RetainedBodyLimit})
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

func (s surface) projectRetainedAgent(ctx context.Context, resource *resources.Resource, rewrite retainedapi.PreparedRewrite) ([]byte, error) {
	projected, err := rewrite.Bind(func(kind, id string) (string, error) {
		if id == resource.UpstreamID {
			return resource.ID, nil
		}
		identity := resource.NativeIdentity()
		identity.Kind = kind
		identity.UpstreamID = id
		bound, findErr := s.store.GetByUpstreamIncludingDeleted(ctx, identity)
		if findErr != nil {
			return "", kernel.RetainedBindingStoreError(findErr)
		}
		return bound.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

func encodeRetainedPage(response map[string]any) ([]byte, *fault.Error) {
	body, err := json.Marshal(response)
	if err != nil {
		return nil, fault.ServerError(500, "internal_error", "The resource list could not be encoded.")
	}
	if len(body) > kernel.RetainedBodyLimit {
		return nil, fault.ServerError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page.")
	}
	return body, nil
}

// Environment and credential references require their own owner-scoped
// contracts. Inline environment values may pass without provider references.
func retainedAgentEnvironment(source oif.Document) *fault.Error {
	v, present := source.Root().Lookup("base_environment")
	if present && v.Kind() != oif.Null && v.Kind() != oif.Object {
		return kernel.PolicyUnavailable("unsupported_stateful_reference", "Native environment references are not supported by this agent resource contract.")
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
			return kernel.PolicyUnavailable("unsupported_stateful_reference", "Native credential references are not supported by this agent resource contract.")
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
