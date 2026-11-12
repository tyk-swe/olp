package probe

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
)

func (p *Prober) certifyRetrieval(ctx context.Context, t Target, model string) error {
	endpoint, err := t.Connection.Transport().RetrievalProbeURL(t.Connection.Transport().Model(model))
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "This binding cannot establish a cost-free native resource metadata probe."}
	}
	status, body, err := p.call(ctx, t, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(t, status, body)
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || doc.Root().Kind() != oif.Object {
		return &Failure{Reason: ProviderProtocolError, Detail: "Invalid native retrieval resource metadata."}
	}
	identity := "name"
	root := doc.Root()
	if t.Connection.Document().Kind == connectors.KindBedrock {
		value, ok := root.Lookup("knowledgeBase")
		if !ok || value.Kind() != oif.Object {
			return &Failure{Reason: ProviderProtocolError, Detail: "Missing Knowledge Base metadata."}
		}
		root = value
		identity = "knowledgeBaseId"
	}
	value, _ := root.Lookup(identity)
	got, ok := value.Text()
	if !ok || got != t.Connection.Transport().Model(model) {
		return &Failure{Reason: ProviderProtocolError, Detail: "Native retrieval metadata does not match the selected resource."}
	}
	return nil
}
