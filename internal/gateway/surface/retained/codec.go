package retained

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

// Decoding follows the endpoint contract, not just its HTTP verb. Multipart
// bodies remain byte-for-byte intact (including boundaries and file content).
func (s surface) readRetainedBody(r *http.Request, e retainedapi.Endpoint) ([]byte, oif.Document, string, *fault.Error) {
	var source oif.Document
	if e.Method != http.MethodPost && e.Method != http.MethodPatch {
		return nil, source, "", nil
	}
	if e.Action == "cancel" && (r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0) {
		return nil, source, "", nil
	}
	if e.Action == "create" && (e.Operation == "skill" || e.Kind == "container_file" && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
			return nil, source, "", fault.ServerError(415, "unsupported_media_type", "Resource uploads require multipart/form-data.")
		}
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			return nil, source, "", fault.ServerError(415, "unsupported_content_encoding", "Multipart uploads require identity encoding.")
		}
		body, err := kernel.ReadBounded(r.Body, s.kernel.Config.MaxBodyBytes)
		if errors.Is(err, kernel.ErrResponseTooLarge) {
			return nil, source, "", ingress.BodyTooLarge()
		}
		if err != nil {
			return nil, source, "", ingress.BodyReadError(err)
		}
		if measured, ok := r.Body.(*ingress.MeasuredBody); ok {
			measured.Decoded = int64(len(body))
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		parts := 0
		var fileID string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, source, "", fault.InvalidRequest("invalid_request", "Invalid multipart upload.", nil)
			}
			name := part.FormName()
			parts++
			if e.Kind == "container_file" && name == "file_id" {
				value, err := kernel.ReadBounded(part, 512)
				if err != nil || part.FileName() != "" || len(value) == 0 {
					return nil, source, "", fault.InvalidRequest("invalid_resource_reference", "A multipart file_id must name one owned file.", nil)
				}
				fileID = string(value)
				continue
			}
			if strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids") || e.Kind == "container_file" && name != "file" {
				return nil, source, "", fault.InvalidRequest("invalid_resource_reference", "Multipart resource references are not supported.", nil)
			}
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, source, "", fault.InvalidRequest("invalid_request", "Invalid multipart upload.", nil)
			}
		}
		if e.Kind == "container_file" {
			if parts != 1 {
				return nil, source, "", fault.InvalidRequest("invalid_request", "Container uploads require exactly one file or file_id.", nil)
			}
			if fileID != "" {
				source, _ = oif.ParseJSON([]byte(`{"file_id":`+strconv.Quote(fileID)+`}`), oif.Limits{})
			}
		}
		return body, source, r.Header.Get("Content-Type"), nil
	}
	body, failure := s.kernel.Gate.ReadBody(r)
	if failure != nil {
		return nil, source, "", failure
	}
	var err error
	source, err = oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.kernel.Config.MaxBodyBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		return nil, source, "", fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	return body, source, "application/json", nil
}

func encodeRetainedRequestBody(e retainedapi.Endpoint, source oif.Document, contentType string) ([]byte, string, error) {
	if e.Kind != "container_file" || !strings.HasPrefix(contentType, "multipart/") {
		return source.Bytes(), contentType, nil
	}
	value, _ := source.Root().Lookup("file_id")
	id, ok := value.Text()
	if !ok || id == "" {
		return nil, "", resources.ErrContract
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("file_id", id); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func bindRetainedModel(source oif.Document, e retainedapi.Endpoint, p *kernel.Pin) (oif.Document, error) {
	if e.Operation == "eval" {
		var changes []oif.Change
		err := walkRetainedEvalModels(source.Root(), "", func(pointer, model string) error {
			changes = append(changes, oif.Change{Pointer: pointer, Value: strconv.Quote(p.Provider.Connector().Model(model)), Origin: oif.ResourceBinding, Reason: "authorized evaluation serving model"})
			return nil
		})
		if err != nil {
			return oif.Document{}, err
		}
		return oif.Apply(source, changes)
	}
	field := "model"
	if e.Kind == "vertex_tuning_job" {
		field = "baseModel"
	} else if e.Operation != "compaction" && e.Operation != "context_cache" && e.Kind != "fine_tuning_job" {
		return source, nil
	}
	if e.Action != "create" && e.Operation != "compaction" {
		return source, nil
	}
	model := p.Provider.Connector().Model(p.Model)
	if e.Kind == "context_cache" {
		model = "models/" + strings.TrimPrefix(model, "models/")
	}
	return oif.Apply(source, []oif.Change{{Pointer: "/" + field, Value: strconv.Quote(model), Origin: oif.ResourceBinding, Reason: "selected route model"}})
}

// hasItemReference reports whether any nested input item references
// provider-held content by native ID. Such IDs name another caller's
// retained items under the shared provider credential, so the variant
// is refused until it can be resolved through owner-scoped bindings.
func hasItemReference(v oif.Value) bool {
	switch v.Kind() {
	case oif.Array:
		for _, child := range v.Elements() {
			if hasItemReference(child) {
				return true
			}
		}
	case oif.Object:
		// Item references permit type to be omitted or null; an
		// untyped item carrying an id is still a provider reference.
		kind := ""
		if t, ok := v.Lookup("type"); ok {
			if text, isText := t.Text(); isText {
				kind = text
			}
		}
		if _, hasID := v.Lookup("id"); hasID && (kind == "" || kind == "item_reference") {
			return true
		}
		for _, field := range v.Members() {
			switch field.Name {
			case "metadata", "attributes", "schema", "json_schema", "parameters", "input_schema", "item_schema":
				continue
			}
			if hasItemReference(field.Value) {
				return true
			}
		}
	}
	return false
}

// retainedFileIDs collects the file handles a request body references,
// in first-seen order, so pin selection can bind to their owner.
func retainedFileIDs(v oif.Value) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(oif.Value)
	walk = func(value oif.Value) {
		switch value.Kind() {
		case oif.Array:
			for _, child := range value.Elements() {
				walk(child)
			}
		case oif.Object:
			for _, field := range value.Members() {
				if field.Name == "file_id" {
					if id, ok := field.Value.Text(); ok && id != "" && !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
					continue
				}
				walk(field.Value)
			}
		}
	}
	walk(v)
	return out
}

// referencedResources resolves the retained resources a request body
// names through the endpoint's declared references, so pin selection
// can bind to their serving identity before dispatch.
func (s surface) referencedResources(ctx context.Context, x *kernel.Call, source oif.Document, e retainedapi.Endpoint) ([]*resources.Resource, error) {
	var out []*resources.Resource
	seen := map[string]bool{}
	resolve := func(kind, id string) error {
		key := kind + "\x00" + id
		if seen[key] {
			return nil
		}
		seen[key] = true
		var bound *resources.Resource
		var err error
		switch kind {
		case resources.KindResponse:
			bound, _, err = s.kernel.ReadResponseResource(ctx, x.KeyID(), id)
		case resources.KindFile:
			bound, _, err = s.kernel.ReadDurable(ctx, kernel.DurableKind(kind, id), x.KeyID(), id)
		case resources.KindGeminiFile:
			local, localErr := retainedGeminiFileID(id)
			if localErr != nil {
				return localErr
			}
			bound, err = s.store.Get(ctx, kind, x.KeyID(), local)
		default:
			bound, err = s.store.Get(ctx, kind, x.KeyID(), id)
		}
		if err != nil {
			return err
		}
		out = append(out, bound)
		return nil
	}
	_, err := retainedapi.Rewrite(source, kernel.RetainedRequestRefsForSource(e, source), func(kind, id string) (string, error) {
		if err := resolve(kind, id); err != nil {
			return "", err
		}
		return id, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s surface) rewriteRetainedRequest(ctx context.Context, x *kernel.Call, a keyauthority.Authority, p *kernel.Pin, route *routing.Route, source oif.Document, e retainedapi.Endpoint) (oif.Document, error) {
	if (e.Operation == "conversation" || e.Operation == "compaction") && hasItemReference(source.Root()) {
		return oif.Document{}, errors.New("item_reference inputs are not supported")
	}
	rewritten, err := retainedapi.Rewrite(source, kernel.RetainedRequestRefsForSource(e, source), func(kind, id string) (string, error) {
		if s.store == nil {
			return "", resources.ErrNotFound
		}
		var bound *resources.Resource
		var err error
		if kind == resources.KindResponse {
			var contract *kernel.StoredResponseContract
			bound, contract, err = s.kernel.ReadResponseResource(ctx, x.KeyID(), id)
			if err == nil {
				_, _, failure := s.kernel.AdmitStoredResponse(ctx, x, a, bound, contract, kernel.RetainedNewWork)
				if failure != nil {
					err = resources.ErrNotFound
				}
			}
		} else if kind == resources.KindFile {
			// A file handle may name either the transformed or the
			// strict durable kind; resolve it through the durable
			// contract so strict handles keep their checks.
			var contract *kernel.DurableDocument
			bound, contract, err = s.kernel.ReadDurable(ctx, kernel.DurableKind(kind, id), x.KeyID(), id)
			if err == nil {
				var failure *fault.Error
				if contract != nil {
					_, _, failure = s.kernel.AdmitDurable(ctx, x, a, bound, contract, x.OperationName(), kernel.RetainedNewWork)
				} else {
					_, _, failure = s.kernel.ResolveResource(ctx, x, a, bound, x.OperationName(), kernel.RetainedNewWork)
				}
				if failure != nil {
					err = resources.ErrNotFound
				}
			}
		} else {
			local := id
			if kind == resources.KindGeminiFile {
				local, err = retainedGeminiFileID(id)
				if err != nil {
					return "", err
				}
			}
			bound, err = s.store.Get(ctx, kind, x.KeyID(), local)
			if err == nil {
				_, _, failure := s.kernel.ResolveResource(ctx, x, a, bound, x.OperationName(), kernel.RetainedNewWork)
				if failure != nil {
					err = resources.ErrNotFound
				}
			}
		}
		if err != nil || !kernel.RetainedAffinity(bound, p, route) || kernel.RetainedUnsettled(bound.State) {
			return "", resources.ErrNotFound
		}
		if kind == resources.KindGeminiFile {
			return retainedGeminiFileURI(bound, p)
		}
		return bound.UpstreamID, nil
	})
	if err != nil {
		return oif.Document{}, err
	}
	return bindRetainedModel(rewritten, e, p)
}

// Only declared pagination/filter fields reach the provider. ID cursors have
// exactly the same owner, parent and serving affinity checks as path IDs.
func (s surface) retainedQuery(ctx context.Context, x *kernel.Call, e retainedapi.Endpoint, resource *resources.Resource, raw url.Values) (url.Values, *fault.Error) {
	out := url.Values{}
	for name, values := range raw {
		if len(values) != 1 {
			return nil, fault.InvalidRequest("invalid_parameter", "Repeated query parameters are not supported.", &name)
		}
		v := values[0]
		switch name {
		case "key":
			if e.Surface != "gemini" || v == "" {
				return nil, fault.InvalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
			continue
		case "limit", "page_size", "pageSize":
			n, err := strconv.Atoi(v)
			if e.Action != "list" || err != nil || n < 1 || n > 100 {
				return nil, fault.InvalidRequest("invalid_parameter", "Page size must be between 1 and 100.", &name)
			}
		case "order":
			if e.Action != "list" || (v != "asc" && v != "desc") {
				return nil, fault.InvalidRequest("invalid_parameter", "order must be asc or desc.", &name)
			}
		case "filter":
			if e.Action != "list" || e.Kind != "vector_store_file" || !strings.Contains("|in_progress|completed|failed|cancelled|", "|"+v+"|") {
				return nil, fault.InvalidRequest("invalid_parameter", "Unsupported list filter.", &name)
			}
		case "after", "before", "after_id", "before_id":
			if e.Action != "list" {
				return nil, fault.InvalidRequest("invalid_parameter", "Cursor is only valid on a collection.", &name)
			}
			bound, err := s.store.Get(ctx, e.Kind, x.KeyID(), v)
			if err != nil || resource == nil || bound.ParentID == nil || *bound.ParentID != resource.UUID || bound.RouteRevisionID != resource.RouteRevisionID || !kernel.SamePin(bound, resource) {
				return nil, fault.InvalidRequest("invalid_resource_reference", "The cursor is unavailable under this parent.", &name)
			}
			v = bound.UpstreamID
		case "beta":
			// The pinned Anthropic SDK appends ?beta=true to every
			// Skills and Skill Versions request; forward it unchanged.
			if e.Operation != "skill" || v != "true" {
				return nil, fault.InvalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
		case "page":
			// Skills lists paginate with the provider's opaque page
			// token (the previous response's next_page), not a gateway
			// resource ID, so it passes through without a binding lookup.
			if e.Operation != "skill" || e.Action != "list" || v == "" {
				return nil, fault.InvalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
		case "updateMask":
			if e.Kind != "context_cache" || e.Action != "patch" {
				return nil, fault.InvalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
			for _, field := range strings.Split(v, ",") {
				if field != "ttl" && field != "expire_time" && field != "expireTime" {
					return nil, fault.InvalidRequest("invalid_parameter", "Only cache expiration can be updated.", &name)
				}
			}
		default:
			return nil, fault.InvalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
		}
		out.Set(name, v)
	}
	return out, nil
}
