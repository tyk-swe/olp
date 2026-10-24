package providers

import (
	"context"
	"github.com/tyk-swe/olp/internal/oif"
	"net/http"
)

func (s *Server) certifyRetrieval(ctx context.Context, cfg *Configuration, credential []byte, model string) error {
	endpoint, err := cfg.transport().RetrievalProbeURL(cfg.transport().Model(model))
	if err != nil {
		return &probeError{Code: "capability_unavailable", Detail: "This binding cannot establish a cost-free native resource metadata probe."}
	}
	status, body, err := s.call(ctx, cfg, credential, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(cfg, status, body)
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || doc.Root().Kind() != oif.Object {
		return &probeError{Code: "provider_protocol_error", Detail: "Invalid native retrieval resource metadata."}
	}
	identity := "name"
	root := doc.Root()
	if cfg.Kind == KindBedrock {
		value, ok := root.Lookup("knowledgeBase")
		if !ok || value.Kind() != oif.Object {
			return &probeError{Code: "provider_protocol_error", Detail: "Missing Knowledge Base metadata."}
		}
		root = value
		identity = "knowledgeBaseId"
	}
	value, _ := root.Lookup(identity)
	got, ok := value.Text()
	if !ok || got != cfg.transport().Model(model) {
		return &probeError{Code: "provider_protocol_error", Detail: "Native retrieval metadata does not match the selected resource."}
	}
	return nil
}
