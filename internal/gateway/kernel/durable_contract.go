package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
)

type DurableDocument struct {
	Version   string              `json:"version"`
	Source    []byte              `json:"source"`
	Effective []byte              `json:"effective,omitempty"`
	Result    []byte              `json:"result,omitempty"`
	Binding   string              `json:"binding"`
	Serving   oif.ServingIdentity `json:"serving"`
	Asset     *DurableAsset       `json:"asset,omitempty"`
}

type DurableAsset struct {
	SHA256      string `json:"sha256"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Endpoint    string `json:"endpoint"`
	Size        int64  `json:"size"`
	ItemCount   int    `json:"item_count"`
}

func DurableKind(prefix string, id string) string {
	if strings.HasPrefix(id, "strict_"+prefix+"_") {
		return "strict_" + prefix
	}
	return prefix
}

func validDurableDocument(doc DurableDocument, r *resources.Resource) bool {
	if doc.Version != resources.DurableContractVersion || doc.Binding == "" || doc.Binding != ResourceModel(r) || doc.Serving.ProviderID != r.ProviderID || doc.Serving.RevisionID != r.ProviderRevisionID || doc.Serving.Model != doc.Binding || len(doc.Source) == 0 {
		return false
	}
	if _, err := oif.ParseJSON(doc.Source, oif.Limits{MaxBytes: resources.MaxContinuationBytes}); err != nil {
		return false
	}
	if len(doc.Effective) > 0 {
		if _, err := oif.ParseJSON(doc.Effective, oif.Limits{MaxBytes: resources.MaxContinuationBytes}); err != nil {
			return false
		}
	}
	if len(doc.Result) > 0 {
		if _, err := oif.ParseJSON(doc.Result, oif.Limits{MaxBytes: resources.MaxContinuationBytes}); err != nil {
			return false
		}
	}
	return true
}

func (s *Server) ReadDurable(ctx context.Context, kind, owner, id string) (*resources.Resource, *DurableDocument, error) {
	if kind != resources.KindStrictBatch && kind != resources.KindStrictFile {
		r, err := s.Deps.Resources.Get(ctx, kind, owner, id)
		return r, nil, err
	}
	r, payload, err := s.Deps.Resources.ReadDurableContract(ctx, kind, owner, id)
	if err != nil {
		return nil, nil, err
	}
	var doc DurableDocument
	if json.Unmarshal(payload, &doc) != nil || !validDurableDocument(doc, r) {
		return nil, nil, resources.ErrContract
	}
	return r, &doc, nil
}

func (s *Server) DurableExpiry(provider *time.Time) *time.Time {
	expires := s.Now().Add(resources.DurableLifetime - time.Second)
	if provider != nil && provider.Before(expires) {
		expires = *provider
	}
	return &expires
}

func DurableMetadata(model string) []byte {
	encoded, _ := json.Marshal(map[string]string{"upstream_model": model})
	return encoded
}

func DurableError(err error) *fault.Error {
	if errors.Is(err, resources.ErrPayloadTooLarge) || errors.Is(err, resources.ErrMetadataTooLarge) {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The provider resource exceeded the durable state bound.")
	}
	return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The accepted provider resource could not be committed.")
}
