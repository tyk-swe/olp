package providers

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/internal/oif"
)

// Certification reads the native collection; it never creates a billable
// resource or infers support from a generic 404 response.
func (s *Server) certifyAgentResources(ctx context.Context, cfg *Configuration, credential []byte, model string) error {
	endpoint, err := cfg.transport().ResourceURL(model, "agents", url.Values{"page_size": {"1"}})
	if err != nil {
		return err
	}
	status, body, err := s.call(ctx, cfg, credential, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(cfg, status, body)
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid agent collection."}
	}
	items, present := source.Root().Lookup("agents")
	// Protobuf JSON can omit an empty repeated field.
	if source.Root().Kind() != oif.Object || present && items.Kind() != oif.Array {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid agent collection."}
	}
	for _, item := range items.Elements() {
		id, _ := item.Lookup("id")
		value, ok := id.Text()
		if !ok || value == "" {
			return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an agent without an identity."}
		}
	}
	return nil
}
