package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

var errRetainedBindingUnavailable = errors.New("retained resource binding unavailable")

func RetainedBindingStoreError(err error) error {
	if errors.Is(err, resources.ErrNotFound) || errors.Is(err, resources.ErrContract) {
		return err
	}
	return fmt.Errorf("%w: %w", errRetainedBindingUnavailable, err)
}

func RetainedProjectionError(exchange *PinnedExchange, err error) *fault.Error {
	if errors.Is(err, errRetainedBindingUnavailable) {
		exchange.Interrupt(ClassProtocol, true)
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Resource identity bindings are unavailable.")
	}
	exchange.InvalidNative()
	return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider response contained an unbound resource identity.")
}

func (s *Server) RetainedBodyDigest(e retainedapi.Endpoint, slug string, body []byte, contentType string) (digest, legacyDigest []byte, failure *fault.Error) {
	if !strings.HasPrefix(contentType, "multipart/") {
		digest, failure := s.RetainedSubmissionDigest(e, slug, body)
		return digest, nil, failure
	}
	digester := s.Deps.Digester
	if digester == nil {
		return nil, nil, fault.ServerError(503, "authority_unavailable", "Resource submission identity is unavailable.")
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, nil, fault.InvalidRequest("invalid_request", "Invalid multipart upload.", nil)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	type partContent struct {
		Header map[string][]string
		Body   []byte
		field  string
	}
	var parts []partContent
	for {
		part, readErr := reader.NextRawPart()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, nil, fault.InvalidRequest("invalid_request", "Invalid multipart upload.", nil)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, ingress.BodyReadError(err)
		}
		parts = append(parts, partContent{Header: part.Header, Body: data, field: multipartMetadataField(part)})
	}
	compare := func(a, b partContent) int {
		if a.field == "" || b.field == "" {
			return strings.Compare(b.field, a.field)
		}
		return strings.Compare(a.field, b.field)
	}
	digestParts := func(ordered []partContent) []byte {
		encoded, _ := json.Marshal(ordered)
		return digester.Digest(secrets.RetainedSubmissionDigest, e.Operation+"\x00"+e.Kind+"\x00"+slug+"\x00"+string(encoded))
	}
	if slices.IsSortedFunc(parts, compare) {
		return digestParts(parts), nil, nil
	}
	canonical := slices.SortedStableFunc(slices.Values(parts), compare)
	return digestParts(canonical), digestParts(parts), nil
}

func multipartMetadataField(part *multipart.Part) string {
	disposition, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	name := params["name"]
	if _, hasFilename := params["filename"]; err != nil || disposition != "form-data" || hasFilename || name == "file" || name == "files[]" {
		return ""
	}
	return name
}

// samePin reports whether two resources share one serving pin.
func SamePin(a, b *resources.Resource) bool {
	return a.ProviderID == b.ProviderID && a.ProviderRevisionID == b.ProviderRevisionID && a.TargetID == b.TargetID && a.SlotID == b.SlotID && sameCredential(a.CredentialID, b.CredentialID)
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

func RetainedRequestRefsForSource(e retainedapi.Endpoint, source oif.Document) map[string]string {
	refs := retainedRequestRefs(e)
	if e.Operation == "eval" {
		add := func(root oif.Value, path string) {
			data, _ := root.Lookup("data_source")
			value, _ := data.Lookup("source")
			kind, _ := value.Lookup("type")
			switch text, _ := kind.Text(); text {
			case "file_id":
				refs[path+"/data_source/source/id"] = resources.KindFile
			case "file_content":
				refs[path+"/data_source/source/content"] = retainedapi.Opaque
			}
		}
		add(source.Root(), "")
		if e.Action == "list" {
			items, _ := source.Root().Lookup(e.Collection)
			for i, item := range items.Elements() {
				add(item, "/"+e.Collection+"/"+strconv.Itoa(i))
			}
		}
	}
	return refs
}

func RetainedResponseRefs(e retainedapi.Endpoint, source oif.Document) map[string]string {
	refs := RetainedRequestRefsForSource(e, source)
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
		addCriteria := func(root oif.Value, path string) {
			criteria, _ := root.Lookup("testing_criteria")
			for i := range criteria.Elements() {
				refs[path+"/testing_criteria/"+strconv.Itoa(i)+"/id"] = retainedapi.Literal
			}
		}
		addCriteria(source.Root(), "")
		if e.Action == "list" {
			items, _ := source.Root().Lookup(e.Collection)
			for i, item := range items.Elements() {
				addCriteria(item, "/"+e.Collection+"/"+strconv.Itoa(i))
			}
		}
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

func RetainedCreateIDs(e retainedapi.Endpoint, body []byte) ([]string, error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: RetainedBodyLimit})
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

// Children inherit serving authority, while lifecycle facts belong to the
// native child observed under that parent.
func retainedDiscoveredChild(parent *resources.Resource, kind, id string, item oif.Value, e retainedapi.Endpoint, now time.Time) (*resources.Resource, error) {
	child := *parent
	child.UUID, child.ID = [16]byte{}, ""
	child.Kind, child.UpstreamID, child.State = kind, id, "active"
	child.ParentID, child.SubmissionID = &parent.UUID, nil
	child.ExpiresAt, child.RetainedAccounting = nil, nil
	child.CreatedAt, child.UpdatedAt = time.Time{}, time.Time{}
	var inherited map[string]json.RawMessage
	if json.Unmarshal(parent.Metadata, &inherited) != nil {
		return nil, resources.ErrContract
	}
	metadata := map[string]json.RawMessage{}
	for _, field := range []string{"upstream_model", "contract_revision", "authorized_models", "serving_models", "memory_tier"} {
		if value, present := inherited[field]; present {
			metadata[field] = value
		}
	}
	child.Metadata, _ = json.Marshal(metadata)
	if item.Kind() == oif.Object {
		document, err := oif.ParseJSON([]byte(item.Raw()), oif.Limits{MaxBytes: RetainedBodyLimit})
		if err != nil {
			return nil, err
		}
		if err := resources.EnrichRetained(&child, e, document, now); err != nil {
			return nil, err
		}
	}
	return &child, nil
}

// Project only declared identities. Primary children observed under an
// authorized parent may be discovered; unrelated references must already exist.
func (s *Server) ProjectRetainedGeneric(ctx context.Context, resource *resources.Resource, e retainedapi.Endpoint, source oif.Document, rewrite retainedapi.PreparedRewrite) ([]byte, error) {
	nativeItems := map[string]oif.Value{}
	if e.Action == "list" {
		items, _ := source.Root().Lookup(e.Collection)
		for _, item := range items.Elements() {
			identity, _ := item.Lookup(e.Identity)
			if id, valid := identity.Text(); valid {
				nativeItems[id] = item
			}
		}
	}
	resultFiles := map[string]bool{}
	if e.Operation == "fine_tuning" {
		if files, present := source.Root().Lookup("result_files"); present && files.Kind() == oif.Array {
			for _, file := range files.Elements() {
				if id, ok := file.Text(); ok {
					resultFiles[id] = true
				}
			}
		}
	}
	projected, err := rewrite.Bind(func(kind, id string) (string, error) {
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
			parent, findErr := s.Deps.Resources.GetByUUID(ctx, *resource.ParentID, resource.APIKeyID)
			if findErr != nil {
				return "", RetainedBindingStoreError(findErr)
			}
			if parent.Kind != kind || parent.UpstreamID != id {
				return "", resources.ErrNotFound
			}
			return parent.ID, nil
		}
		bound, findErr := s.retainedResultReference(ctx, e, resource, identity)
		if errors.Is(findErr, resources.ErrNotFound) && kind == resources.KindFile {
			// An echoed file reference may name the strict durable
			// kind under the same serving identity.
			strictIdentity := identity
			strictIdentity.Kind = resources.KindStrictFile
			bound, findErr = s.retainedResultReference(ctx, e, resource, strictIdentity)
		}
		resultFile := kind == resources.KindFile && resultFiles[id]
		if errors.Is(findErr, resources.ErrNotFound) && (kind == e.Kind && e.Action == "list" && len(e.Ancestors) > 0 || kind == "skill_version" && resource.Kind == "skill" || resultFile) {
			child, childErr := retainedDiscoveredChild(resource, kind, id, nativeItems[id], e, s.Now())
			if childErr != nil {
				return "", childErr
			}
			if resultFile {
				child.ContractVersion = nil
				child.Metadata, _ = json.Marshal(map[string]string{"upstream_model": ResourceModel(resource), "file_operation": "fine_tuning", "purpose": "fine-tune-results"})
			}
			var submission []uuid.UUID
			if kind == "eval_run" {
				metadata, _ := nativeItems[id].Lookup("metadata")
				if marker, present := metadata.Lookup(resources.RetainedSubmissionMarkerField); present && marker.Kind() != oif.Null {
					text, valid := marker.Text()
					original, err := uuid.Parse(text)
					if !valid || err != nil || original == uuid.Nil || original.String() != text {
						return "", resources.ErrContract
					}
					submission = append(submission, original)
				}
			}
			bound, findErr = s.Deps.Resources.BindDiscovered(ctx, child, submission...)
		}
		if findErr != nil {
			return "", RetainedBindingStoreError(findErr)
		}
		if kind == "fine_tuning_checkpoint" && nativeItems[id].Kind() == oif.Object {
			checkpoint, err := oif.ParseJSON([]byte(nativeItems[id].Raw()), oif.Limits{MaxBytes: RetainedBodyLimit})
			if err != nil {
				return "", err
			}
			if err := s.Deps.Resources.DiscoverRetainedModels(ctx, bound, checkpoint); err != nil {
				return "", RetainedBindingStoreError(err)
			}
		}
		return bound.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

func (s *Server) retainedResultReference(ctx context.Context, e retainedapi.Endpoint, owner *resources.Resource, identity resources.NativeIdentity) (*resources.Resource, error) {
	if e.Operation == "retrieval" {
		return s.Deps.Resources.GetByUpstream(ctx, identity)
	}
	return s.Deps.Resources.GetReferenceOf(ctx, owner, identity)
}
