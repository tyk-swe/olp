package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

type durableDocument struct {
	Version   string              `json:"version"`
	Source    []byte              `json:"source"`
	Effective []byte              `json:"effective,omitempty"`
	Result    []byte              `json:"result,omitempty"`
	Binding   string              `json:"binding"`
	Serving   oif.ServingIdentity `json:"serving"`
	Asset     *durableAsset       `json:"asset,omitempty"`
}

type durableAsset struct {
	SHA256      string `json:"sha256"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Endpoint    string `json:"endpoint"`
	// Purpose is the native upload purpose committed with the file. Batch
	// inputs carry "batch"; generation admission requires a purpose the
	// provider profile lists as an inference-file purpose.
	Purpose   string `json:"purpose,omitempty"`
	Size      int64  `json:"size"`
	ItemCount int    `json:"item_count"`
}

// durableOperation names the route operation a committed file contract was
// admitted under. Inference files resolve through generation; every other
// strict file remains on the batch path.
func durableOperation(doc *durableDocument) string {
	if doc != nil && doc.Asset != nil && doc.Asset.Purpose != "" && doc.Asset.Purpose != "batch" {
		return "generation"
	}
	return "batch"
}

// The file is already staged by the bounded media owner. Inspection never
// rewrites its bytes; it only refuses unqualified mixed endpoints/models and
// ambiguous per-item identities before an effectful provider upload.
func (s *Server) validateBatchInput(part *media.Part, model string, provider *runtime.Provider, route *runtime.Route) (string, int, error) {
	opened, err := s.transport().Spool.Open(part.Handle)
	if err != nil {
		return "", 0, err
	}
	defer opened.File.Close()
	scanner := bufio.NewScanner(opened.File)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	seen := map[string]struct{}{}
	endpoint := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || len(seen) >= 10000 {
			return "", 0, resources.ErrContract
		}
		doc, err := oif.ParseJSON(line, oif.Limits{MaxBytes: 1 << 20})
		if err != nil || doc.Root().Kind() != oif.Object {
			return "", 0, resources.ErrContract
		}
		get := func(parent oif.Value, name string) (string, bool) {
			field, ok := parent.Lookup(name)
			text, valid := field.Text()
			return text, ok && valid && text != ""
		}
		custom, ok := get(doc.Root(), "custom_id")
		if !ok || len(custom) > 128 {
			return "", 0, resources.ErrContract
		}
		if _, duplicate := seen[custom]; duplicate {
			return "", 0, resources.ErrContract
		}
		seen[custom] = struct{}{}
		method, ok := get(doc.Root(), "method")
		if !ok || method != http.MethodPost {
			return "", 0, resources.ErrContract
		}
		path, ok := get(doc.Root(), "url")
		if !ok || path != "/v1/chat/completions" && path != "/v1/responses" && path != "/v1/embeddings" || endpoint != "" && endpoint != path {
			return "", 0, resources.ErrContract
		}
		operation := "generation"
		if path == "/v1/embeddings" {
			operation = "embeddings"
		}
		profile, err := provider.Connector().Profile()
		if err != nil || !slices.Contains(route.Operations, operation) || !provider.Supports(model, operation, "openai", "unary") ||
			!provider.Connector().Supports(operation, "openai", "unary") ||
			path == "/v1/chat/completions" && profile.Dialect != "openai-chat" ||
			path == "/v1/responses" && profile.Dialect != "openai-responses" ||
			path == "/v1/embeddings" && profile.OperationDialect("embeddings") != "openai-embeddings" {
			return "", 0, resources.ErrContract
		}
		endpoint = path
		body, ok := doc.Root().Lookup("body")
		if !ok || body.Kind() != oif.Object {
			return "", 0, resources.ErrContract
		}
		itemModel, ok := get(body, "model")
		if !ok || itemModel != model {
			return "", 0, resources.ErrContract
		}
	}
	if scanner.Err() != nil || len(seen) == 0 || opened.Artifact.ContentLength != part.Size || opened.Artifact.Digest != part.Digest {
		return "", 0, resources.ErrContract
	}
	return endpoint, len(seen), nil
}

func durableKind(prefix string, id string) string {
	if strings.HasPrefix(id, "strict_"+prefix+"_") {
		return "strict_" + prefix
	}
	return prefix
}

func validDurableDocument(doc durableDocument, r *resources.Resource) bool {
	if doc.Version != resources.DurableContractVersion || doc.Binding == "" || doc.Binding != resourceModel(r) || doc.Serving.ProviderID != r.ProviderID || doc.Serving.RevisionID != r.ProviderRevisionID || doc.Serving.Model != doc.Binding || len(doc.Source) == 0 {
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

func (s *Server) readDurable(ctx context.Context, kind, owner, id string) (*resources.Resource, *durableDocument, error) {
	if kind != resources.KindStrictBatch && kind != resources.KindStrictFile {
		r, err := s.Resources.Get(ctx, kind, owner, id)
		return r, nil, err
	}
	r, payload, err := s.Resources.ReadDurableContract(ctx, kind, owner, id)
	if err != nil {
		return nil, nil, err
	}
	var doc durableDocument
	if json.Unmarshal(payload, &doc) != nil || !validDurableDocument(doc, r) {
		return nil, nil, resources.ErrContract
	}
	return r, &doc, nil
}

func (s *Server) authorizeDurable(ctx context.Context, x *execution, authority access.Authority, r *resources.Resource, doc *durableDocument, operation string) *Error {
	if doc == nil {
		return nil
	}
	current, ok := x.snapshot().Routes[r.RouteSlug]
	if !ok || !authority.Policy.AllowProviderState || !authority.Allows("inference", current.Slug, current.ProjectID, s.now()) {
		return notFoundError("not_found", "The stored provider resource is unavailable to this key.")
	}
	p, _, e := s.resolveResource(ctx, x, authority, r, operation)
	if e != nil {
		return e
	}
	profile, profileErr := p.provider.Connector().Profile()
	if profileErr != nil || !p.provider.Enabled || !p.slot.Allows(p.model, r.RouteSlug, authority.ID) || p.model != doc.Binding || p.provider.RevisionID != doc.Serving.RevisionID || profile.ID != doc.Serving.ProfileID || profile.Revision != doc.Serving.ProfileRevision {
		return pinUnavailable()
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.provider); err != nil {
			return pinUnavailable()
		}
	}
	return nil
}

func strictResult(body []byte, expectedID string) (oif.Document, string, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: resources.MaxContinuationBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Document{}, "", resources.ErrContract
	}
	id, ok := doc.Root().Lookup("id")
	value, valid := id.Text()
	if !ok || !valid || value == "" || len(value) > 512 || expectedID != "" && value != expectedID {
		return oif.Document{}, "", resources.ErrContract
	}
	return doc, value, nil
}

func strictBatchResult(body []byte, expectedID string) (oif.Document, string, error) {
	doc, id, err := strictResult(body, expectedID)
	if err != nil {
		return oif.Document{}, "", err
	}
	status, ok := doc.Root().Lookup("status")
	state, valid := status.Text()
	if !ok || !valid || !slices.Contains([]string{"validating", "in_progress", "finalizing", "completed", "failed", "expired", "cancelling", "cancelled"}, state) {
		return oif.Document{}, "", resources.ErrContract
	}
	for _, name := range []string{"input_file_id", "output_file_id", "error_file_id"} {
		field, present := doc.Root().Lookup(name)
		if !present || field.Kind() == oif.Null {
			continue
		}
		value, valid := field.Text()
		if !valid || value == "" || len(value) > 512 {
			return oif.Document{}, "", resources.ErrContract
		}
	}
	if counts, present := doc.Root().Lookup("request_counts"); present && counts.Kind() != oif.Null {
		if counts.Kind() != oif.Object {
			return oif.Document{}, "", resources.ErrContract
		}
		for _, name := range []string{"total", "completed", "failed"} {
			if value, present := counts.Lookup(name); present {
				number, err := strconv.ParseInt(value.Raw(), 10, 64)
				if err != nil || number < 0 {
					return oif.Document{}, "", resources.ErrContract
				}
			}
		}
	}
	return doc, id, nil
}

func strictBatchInputMatches(result oif.Document, effective []byte) bool {
	bound, err := oif.ParseJSON(effective, oif.Limits{MaxBytes: resources.MaxContinuationBytes})
	if err != nil || bound.Root().Kind() != oif.Object {
		return false
	}
	expected, present := bound.Root().Lookup("input_file_id")
	fileID, valid := expected.Text()
	if !present || !valid || fileID == "" {
		return false
	}
	if observed, present := result.Root().Lookup("input_file_id"); present && observed.Kind() != oif.Null {
		value, valid := observed.Text()
		return valid && value == fileID
	}
	return true
}

func strictState(doc oif.Document, fallback string) string {
	status, ok := doc.Root().Lookup("status")
	if ok {
		if value, valid := status.Text(); valid && value != "" && len(value) <= 64 {
			return value
		}
	}
	return fallback
}

func (s *Server) durableExpiry(provider *time.Time) *time.Time {
	expires := s.now().Add(resources.DurableLifetime - time.Second)
	if provider != nil && provider.Before(expires) {
		expires = *provider
	}
	return &expires
}

func durableMetadata(model string) []byte {
	encoded, _ := json.Marshal(map[string]string{"upstream_model": model})
	return encoded
}

func durableError(err error) *Error {
	if errors.Is(err, resources.ErrPayloadTooLarge) || errors.Is(err, resources.ErrMetadataTooLarge) {
		return serverError(http.StatusBadGateway, "upstream_response_too_large", "The provider resource exceeded the durable state bound.")
	}
	return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The accepted provider resource could not be committed.")
}

func durableProjection(result []byte, localID string, files map[string]string) ([]byte, error) {
	doc, _, err := strictResult(result, "")
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(localID)
	changes := []oif.Change{{Pointer: "/id", Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped provider resource"}}
	for field, mapped := range files {
		if _, present := doc.Root().Lookup(field); present {
			encoded, _ := json.Marshal(mapped)
			changes = append(changes, oif.Change{Pointer: "/" + field, Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped provider file"})
		}
	}
	out, err := oif.Apply(doc, changes)
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func strictIdentityProjection(result []byte, upstreamID, localID string) ([]byte, error) {
	if _, _, err := strictResult(result, upstreamID); err != nil {
		return nil, err
	}
	return durableProjection(result, localID, nil)
}
