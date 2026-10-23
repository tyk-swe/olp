package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

// Decoding follows the endpoint contract, not just its HTTP verb. Multipart
// bodies remain byte-for-byte intact (including boundaries and file content).
func (s *Server) readRetainedBody(r *http.Request, e retainedapi.Endpoint) ([]byte, oif.Document, string, *Error) {
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
			return nil, source, "", serverError(415, "unsupported_media_type", "Skill uploads require multipart/form-data.")
		}
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			return nil, source, "", serverError(415, "unsupported_content_encoding", "Multipart uploads require identity encoding.")
		}
		body, err := readBounded(r.Body, s.cfg.MaxBodyBytes)
		if errors.Is(err, errResponseTooLarge) {
			return nil, source, "", bodyTooLarge()
		}
		if err != nil {
			return nil, source, "", bodyReadError(err)
		}
		if measured, ok := r.Body.(*measuredBody); ok {
			measured.decoded = int64(len(body))
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, source, "", invalidRequest("invalid_request", "Invalid multipart upload.", nil)
			}
			name := part.FormName()
			if strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids") || e.Kind == "container_file" && name != "file" {
				return nil, source, "", invalidRequest("invalid_resource_reference", "Multipart resource references are not supported.", nil)
			}
			if _, err = io.Copy(io.Discard, part); err != nil {
				return nil, source, "", invalidRequest("invalid_request", "Invalid multipart upload.", nil)
			}
		}
		return body, source, r.Header.Get("Content-Type"), nil
	}
	body, failure := s.readBody(r)
	if failure != nil {
		return nil, source, "", failure
	}
	var err error
	source, err = oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		return nil, source, "", invalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	return body, source, "application/json", nil
}

func (s *Server) retainedBodyDigest(e retainedapi.Endpoint, slug string, body []byte, contentType string) ([]byte, *Error) {
	if !strings.HasPrefix(contentType, "multipart/") {
		return s.retainedSubmissionDigest(e, slug, body)
	}
	digester, ok := s.Runtime.(interface {
		Digest(secrets.DigestPurpose, string) []byte
	})
	if !ok {
		return nil, serverError(503, "authority_unavailable", "Resource submission identity is unavailable.")
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, invalidRequest("invalid_request", "Invalid multipart upload.", nil)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	type partContent struct {
		Header map[string][]string
		Body   []byte
	}
	var parts []partContent
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalidRequest("invalid_request", "Invalid multipart upload.", nil)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, bodyReadError(err)
		}
		parts = append(parts, partContent{Header: part.Header, Body: data})
	}
	canonical, err := json.Marshal(parts)
	if err != nil {
		return nil, invalidRequest("invalid_request", "Invalid multipart upload.", nil)
	}
	return digester.Digest(secrets.RetainedSubmissionDigest, e.Operation+"\x00"+e.Kind+"\x00"+slug+"\x00"+string(canonical)), nil
}

func bindRetainedModel(source oif.Document, e retainedapi.Endpoint, p *pin) (oif.Document, error) {
	field := "model"
	if e.Kind == "vertex_tuning_job" {
		field = "baseModel"
	} else if e.Operation != "compaction" && e.Operation != "context_cache" && e.Kind != "fine_tuning_job" {
		return source, nil
	}
	if e.Action != "create" && e.Operation != "compaction" {
		return source, nil
	}
	return oif.Apply(source, []oif.Change{{Pointer: "/" + field, Value: strconv.Quote(p.provider.Connector().Model(p.model)), Origin: oif.ResourceBinding, Reason: "selected route model"}})
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
func (s *Server) referencedResources(ctx context.Context, x *call, source oif.Document, e retainedapi.Endpoint) ([]*resources.Resource, error) {
	refs := retainedRequestRefs(e)
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
			bound, _, err = s.readResponseResource(ctx, x.keyID, id)
		case resources.KindFile:
			bound, _, err = s.readDurable(ctx, durableKind(kind, id), x.keyID, id)
		default:
			bound, err = s.Resources.Get(ctx, kind, x.keyID, id)
		}
		if err != nil {
			return err
		}
		out = append(out, bound)
		return nil
	}
	var walk func(oif.Value) error
	walk = func(v oif.Value) error {
		switch v.Kind() {
		case oif.Array:
			for _, child := range v.Elements() {
				if err := walk(child); err != nil {
					return err
				}
			}
		case oif.Object:
			for _, field := range v.Members() {
				switch field.Name {
				case "metadata", "attributes", "schema", "json_schema", "parameters", "input_schema", "item_schema":
					continue
				}
				kind, isRef := refs[field.Name]
				if !isRef || kind == retainedapi.Literal {
					if err := walk(field.Value); err != nil {
						return err
					}
					continue
				}
				values := []oif.Value{field.Value}
				if field.Value.Kind() == oif.Array {
					values = field.Value.Elements()
				}
				for _, value := range values {
					id, ok := value.Text()
					if !ok || id == "" {
						return resources.ErrNotFound
					}
					if err := resolve(kind, id); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(source.Root()); err != nil {
		return nil, err
	}
	return out, nil
}

// samePin reports whether two resources share one serving pin.
func samePin(a, b *resources.Resource) bool {
	return a.ProviderID == b.ProviderID && a.ProviderRevisionID == b.ProviderRevisionID && a.TargetID == b.TargetID && a.SlotID == b.SlotID && sameCredential(a.CredentialID, b.CredentialID)
}

func (s *Server) rewriteRetainedRequest(ctx context.Context, x *call, a access.Authority, p *pin, route *routing.Route, source oif.Document, e retainedapi.Endpoint) (oif.Document, error) {
	if (e.Operation == "conversation" || e.Operation == "compaction") && hasItemReference(source.Root()) {
		return oif.Document{}, errors.New("item_reference inputs are not supported")
	}
	rewritten, err := retainedapi.Rewrite(source, retainedRequestRefs(e), func(kind, id string) (string, error) {
		if s.Resources == nil {
			return "", resources.ErrNotFound
		}
		var bound *resources.Resource
		var err error
		if kind == resources.KindResponse {
			var contract *storedResponseContract
			bound, contract, err = s.readResponseResource(ctx, x.keyID, id)
			if err == nil {
				_, _, failure := s.admitStoredResponse(ctx, x, a, bound, contract, retainedNewWork)
				if failure != nil {
					err = resources.ErrNotFound
				}
			}
		} else if kind == resources.KindFile {
			// A file handle may name either the transformed or the
			// strict durable kind; resolve it through the durable
			// contract so strict handles keep their checks.
			var contract *durableDocument
			bound, contract, err = s.readDurable(ctx, durableKind(kind, id), x.keyID, id)
			if err == nil {
				var failure *Error
				if contract != nil {
					_, _, failure = s.admitDurable(ctx, x, a, bound, contract, x.operationName(), retainedNewWork)
				} else {
					_, _, failure = s.resolveResource(ctx, x, a, bound, x.operationName(), retainedNewWork)
				}
				if failure != nil {
					err = resources.ErrNotFound
				}
			}
		} else {
			bound, err = s.Resources.Get(ctx, kind, x.keyID, id)
			if err == nil {
				_, _, failure := s.resolveResource(ctx, x, a, bound, x.operationName(), retainedNewWork)
				if failure != nil {
					err = resources.ErrNotFound
				}
			}
		}
		if err != nil || !retainedAffinity(bound, p, route) || bound.State == "submitting" || bound.State == "submission_unknown" {
			return "", resources.ErrNotFound
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
func (s *Server) retainedQuery(ctx context.Context, x *call, e retainedapi.Endpoint, resource *resources.Resource, raw url.Values) (url.Values, *Error) {
	out := url.Values{}
	for name, values := range raw {
		if len(values) != 1 {
			return nil, invalidRequest("invalid_parameter", "Repeated query parameters are not supported.", &name)
		}
		v := values[0]
		switch name {
		case "limit", "page_size", "pageSize":
			n, err := strconv.Atoi(v)
			if e.Action != "list" || err != nil || n < 1 || n > 100 {
				return nil, invalidRequest("invalid_parameter", "Page size must be between 1 and 100.", &name)
			}
		case "order":
			if e.Action != "list" || (v != "asc" && v != "desc") {
				return nil, invalidRequest("invalid_parameter", "order must be asc or desc.", &name)
			}
		case "filter":
			if e.Action != "list" || e.Kind != "vector_store_file" || !strings.Contains("|in_progress|completed|failed|cancelled|", "|"+v+"|") {
				return nil, invalidRequest("invalid_parameter", "Unsupported list filter.", &name)
			}
		case "after", "before", "after_id", "before_id":
			if e.Action != "list" {
				return nil, invalidRequest("invalid_parameter", "Cursor is only valid on a collection.", &name)
			}
			bound, err := s.Resources.Get(ctx, e.Kind, x.keyID, v)
			if err != nil || resource == nil || bound.ParentID == nil || *bound.ParentID != resource.UUID || bound.ProviderID != resource.ProviderID || bound.SlotID != resource.SlotID || bound.RouteRevisionID != resource.RouteRevisionID || bound.TargetID != resource.TargetID || !sameCredential(bound.CredentialID, resource.CredentialID) {
				return nil, invalidRequest("invalid_resource_reference", "The cursor is unavailable under this parent.", &name)
			}
			v = bound.UpstreamID
		case "beta":
			// The pinned Anthropic SDK appends ?beta=true to every
			// Skills and Skill Versions request; forward it unchanged.
			if e.Operation != "skill" || v != "true" {
				return nil, invalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
		case "page":
			// Skills lists paginate with the provider's opaque page
			// token (the previous response's next_page), not a gateway
			// resource ID, so it passes through without a binding lookup.
			if e.Operation != "skill" || e.Action != "list" || v == "" {
				return nil, invalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
		case "updateMask":
			if e.Kind != "context_cache" || e.Action != "patch" {
				return nil, invalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
			}
			for _, field := range strings.Split(v, ",") {
				if field != "ttl" && field != "expire_time" && field != "expireTime" {
					return nil, invalidRequest("invalid_parameter", "Only cache expiration can be updated.", &name)
				}
			}
		default:
			return nil, invalidRequest("invalid_parameter", "Unsupported query parameter.", &name)
		}
		out.Set(name, v)
	}
	return out, nil
}

func retainedRequestRefs(e retainedapi.Endpoint) map[string]string {
	refs := map[string]string{}
	switch e.Operation {
	case "fine_tuning":
		refs["training_file"] = resources.KindFile
		refs["validation_file"] = resources.KindFile
	case "vector_store", "container":
		refs["file_ids"] = resources.KindFile
		refs["file_id"] = resources.KindFile
	case "eval":
		refs["file_id"] = resources.KindFile
	case "conversation", "compaction":
		refs["vector_store_id"] = "vector_store"
		refs["file_id"] = resources.KindFile
		refs["previous_response_id"] = resources.KindResponse
		refs["call_id"] = retainedapi.Literal
		refs["item_id"] = retainedapi.Literal
		refs["id"] = retainedapi.Literal
	case "retrieval":
		refs["file_id"] = resources.KindFile
	case "context_cache":
		refs["file_uri"] = resources.KindGeminiFile
		refs["fileUri"] = resources.KindGeminiFile
	}
	return refs
}

func retainedResponseRefs(e retainedapi.Endpoint, source oif.Document) map[string]string {
	refs := retainedRequestRefs(e)
	// Request references are also echoed in native responses. Response-only
	// literals and references are declared per family, never by suffix alone.
	switch e.Operation {
	case "fine_tuning":
		refs["organization_id"] = retainedapi.Literal
		refs["fine_tuning_job_id"] = "fine_tuning_job"
		refs["result_files"] = resources.KindFile
		refs["run_id"] = retainedapi.Literal // external experiment tracker
	case "vector_store":
		refs["vector_store_id"] = "vector_store"
	case "container":
		refs["container_id"] = "container"
	case "eval":
		refs["eval_id"] = "eval"
		refs["run_id"] = "eval_run"
		refs["datasource_item_id"] = retainedapi.Literal
	case "conversation":
		refs["conversation_id"] = "conversation"
		refs["id"] = retainedapi.Literal // message/content item identity
	case "skill":
		refs["skill_id"] = "skill"
		refs["latest_version_id"] = "skill_version"
	case "compaction":
		refs["id"] = retainedapi.Literal  // transient output items
		refs["/id"] = retainedapi.Literal // stateless compaction receipt
	}
	if e.Operation != "compaction" && e.Operation != "retrieval" {
		if e.Action == "list" || e.Kind == "conversation_item" && e.Action == "create" {
			items, _ := source.Root().Lookup(e.Collection)
			for i := range items.Elements() {
				refs["/"+e.Collection+"/"+strconv.Itoa(i)+"/"+e.Identity] = e.Kind
			}
			refs["first_id"], refs["last_id"] = e.Kind, e.Kind
		} else if e.Kind == "conversation_item" && e.Action == "delete" {
			// Deleting a conversation item returns the parent
			// Conversation, so the root identity is the parent's.
			refs["/"+e.Identity] = "conversation"
		} else {
			refs["/"+e.Identity] = e.Kind
		}
	}
	return refs
}

func retainedCreateIDs(e retainedapi.Endpoint, body []byte) ([]string, error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
	if err != nil || source.Root().Kind() != oif.Object {
		return nil, resources.ErrContract
	}
	objects := []oif.Value{source.Root()}
	if e.Kind == "conversation_item" && e.Action == "create" {
		items, ok := source.Root().Lookup(e.Collection)
		if !ok || items.Kind() != oif.Array {
			return nil, resources.ErrContract
		}
		objects = items.Elements()
	}
	ids := make([]string, 0, len(objects))
	seen := map[string]bool{}
	for _, object := range objects {
		value, _ := object.Lookup(e.Identity)
		id, ok := value.Text()
		if !ok || id == "" || len(id) > 512 || seen[id] {
			return nil, resources.ErrContract
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// Project only declared identities. Primary children observed under an
// authorized parent may be discovered; unrelated references must already exist.
func (s *Server) projectRetainedGeneric(ctx context.Context, resource *resources.Resource, e retainedapi.Endpoint, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
	if err != nil {
		return nil, err
	}
	refs := retainedResponseRefs(e, source)
	projected, err := retainedapi.Rewrite(source, refs, func(kind, id string) (string, error) {
		if resource == nil {
			return "", resources.ErrNotFound
		}
		if kind == resource.Kind && id == resource.UpstreamID {
			return resource.ID, nil
		}
		identity := resource.NativeIdentity()
		identity.Kind, identity.UpstreamID = kind, id
		identity.ParentID = nil
		if kind == e.Kind {
			if e.Action == "list" {
				identity.ParentID = &resource.UUID
			} else {
				identity.ParentID = resource.ParentID
			}
		} else if kind == "skill_version" && resource.Kind == "skill" {
			// A Skill response names its latest version, a child
			// observed through the authorized parent Skill.
			identity.ParentID = &resource.UUID
		} else if resource.ParentID != nil && len(e.Ancestors) > 0 && kind == e.Ancestors[len(e.Ancestors)-1] {
			// The parent's own parent may not be nil (eval runs, for example).
			parent, findErr := s.Resources.GetByUUID(ctx, *resource.ParentID, resource.APIKeyID)
			if findErr != nil || parent.Kind != kind || parent.UpstreamID != id {
				return "", resources.ErrNotFound
			}
			return parent.ID, nil
		}
		bound, findErr := s.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
		if errors.Is(findErr, resources.ErrNotFound) && kind == resources.KindFile {
			// An echoed file reference may name the strict durable
			// kind under the same serving identity.
			strictIdentity := identity
			strictIdentity.Kind = resources.KindStrictFile
			bound, findErr = s.Resources.GetByUpstreamIncludingDeleted(ctx, strictIdentity)
		}
		if errors.Is(findErr, resources.ErrNotFound) && (kind == e.Kind && e.Action == "list" && len(e.Ancestors) > 0 || kind == "skill_version" && resource.Kind == "skill") {
			child := *resource
			child.UUID, child.ID = [16]byte{}, ""
			child.Kind, child.UpstreamID, child.State = kind, id, "active"
			child.ParentID, child.SubmissionID = &resource.UUID, nil
			bound, findErr = s.Resources.BindDiscovered(ctx, &child)
		}
		if findErr != nil {
			return "", findErr
		}
		return bound.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}
