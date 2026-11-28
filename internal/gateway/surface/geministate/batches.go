package geministate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s surface) mapUpstreamFile(ctx context.Context, owner *resources.Resource, upstreamName string) (*resources.Resource, error) {
	upstreamID, err := kernel.GeminiFileID(upstreamName)
	if err != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	identity := owner.NativeIdentity()
	identity.Kind, identity.UpstreamID = resources.KindGeminiFile, upstreamID
	if existing, err := s.store.GetByUpstreamIncludingDeleted(ctx, identity); err == nil {
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata := map[string]json.RawMessage{}
	metadata["upstream_model"] = kernel.MustJSON(kernel.ResourceModel(owner))
	metadata["name"] = kernel.MustJSON(upstreamName)
	resource := &resources.Resource{
		Kind: resources.KindGeminiFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
		ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID,
		RouteRevisionID: owner.RouteRevisionID, TargetID: owner.TargetID, SlotID: owner.SlotID,
		CredentialID: owner.CredentialID, UpstreamID: upstreamID, State: "processed",
		Metadata: kernel.MustJSON(metadata), ParentID: new(owner.UUID),
	}
	if owner.ExpiresAt != nil {
		expires := *owner.ExpiresAt
		resource.ExpiresAt = &expires
	}
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	created, err := s.store.Put(commitCtx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.store.GetByUpstreamIncludingDeleted(commitCtx, identity); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func (s surface) batchCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin) *fault.Error) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	localID := strings.TrimPrefix(r.PathValue("name"), "batches/")
	resource, err := s.store.Get(r.Context(), resources.KindGeminiBatch, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini batch mapping could not be read."))
		return
	}
	p, route, pinErr := s.kernel.ResolveResource(r.Context(), x, authority, resource, "batch", use)
	if pinErr != nil {
		s.kernel.Refuse(x, w, pinErr)
		return
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key."))
		return
	}
	if route.Fidelity.Strict() {
		s.kernel.Refuse(x, w, kernel.PolicyUnavailable("target_capability", "Strict routes do not serve Gemini batches."))
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.gates(ctx, x, authority, p, route, false); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if err := op(ctx, x, authority, resource, p); err != nil {
		s.kernel.Refuse(x, w, err)
	}
}

func (s surface) resourceObject(ctx context.Context, resource *resources.Resource, body []byte) ([]byte, error) {
	names, err := readInputNames(resource)
	if err != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	batch, err := upstream.DecodeGeminiBatch(body)
	if err != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	object, err := prepareBatch(body, batch, names)
	if err != nil {
		return nil, err
	}
	return s.projectResource(ctx, resource, object)
}

func (s surface) projectResource(ctx context.Context, resource *resources.Resource, object batchObject) ([]byte, error) {
	outputs := make(map[string]string, len(object.outputs))
	for _, file := range object.outputs {
		mapped, err := s.mapUpstreamFile(ctx, resource, file)
		if err != nil {
			return nil, err
		}
		outputs[file] = "files/" + mapped.ID
	}
	return object.project(resource.ID, resource.RouteSlug, outputs)
}

func (s surface) listBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	pageSize, pageToken, cursorErr := batchPageCursor(r.URL.Query())
	if cursorErr != nil {
		s.kernel.Refuse(x, w, cursorErr)
		return
	}
	rows, continuation, listErr := s.kernel.ListedResourcePage(authority.ID, "pageToken", pageSize, pageToken, func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.store.ListKindsPage(r.Context(), []string{resources.KindGeminiBatch}, authority.ID, count, cursor, false)
		if errors.Is(err, resources.ErrNotFound) {
			param := "pageToken"
			return nil, fault.InvalidRequest("invalid_value", "pageToken must identify a Gemini batch owned by this key.", &param)
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini batch list could not be read.")
		}
		return batch, nil
	}, func(row *resources.Resource) *fault.Error {
		return s.kernel.AuthorizeListedResource(r.Context(), authority, row)
	})
	if listErr != nil {
		s.kernel.Refuse(x, w, listErr)
		return
	}
	operations := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		operation, projectErr := s.resourceObject(r.Context(), row, row.Metadata)
		if projectErr != nil {
			s.kernel.Refuse(x, w, projectionError(projectErr))
			return
		}
		operations = append(operations, operation)
	}
	response := map[string]any{"operations": operations}
	if continuation != "" {
		response["nextPageToken"] = continuation
	}
	s.kernel.WriteStateJSON(w, x, kernel.MustJSON(response))
}

func (s surface) relayBatch(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, method string, cancel bool) {
	s.batchCall(w, r, use, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, resource *resources.Resource, p *kernel.Pin) *fault.Error {
		path := "batches/" + url.PathEscape(resource.UpstreamID)
		if cancel {
			path += ":cancel"
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.kernel.PinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.store.Tombstone(ctx, resource.ID)
				return fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer response.Body.Close()
		body, err := response.ReadBody(s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch response could not be read.")
		}
		if cancel && cancelAcknowledged(body) {
			response.Exchange.Complete(kernel.ClassSuccess)

			projected, projectErr := s.resourceObject(ctx, resource, resource.Metadata)
			if projectErr != nil {
				return projectionError(projectErr)
			}
			s.kernel.WriteStateJSON(w, x, projected, response.Exchange)
			return nil
		}
		batch, decodeErr := upstream.DecodeGeminiBatch(body)
		if decodeErr != nil || batch.ID != resource.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
		}
		names, namesErr := readInputNames(resource)
		if namesErr != nil {
			response.Exchange.Interrupt(kernel.ClassProtocol, true)
			return projectionError(kernel.ErrGeminiBatchFidelity)
		}
		object, metadata, metadataErr := settleResult(response.Exchange, body, batch, kernel.ResourceModel(resource), names)
		if metadataErr != nil {
			return projectionError(kernel.ErrGeminiBatchFidelity)
		}
		if err := s.store.Update(ctx, resource.ID, batch.State, metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch refresh could not be committed.")
		}

		projected, projectErr := s.projectResource(ctx, resource, object)
		if projectErr != nil {
			return projectionError(projectErr)
		}
		s.kernel.WriteStateJSON(w, x, projected, response.Exchange)
		return nil
	})
}

func (s surface) getBatch(w http.ResponseWriter, r *http.Request) {
	s.relayBatch(w, r, kernel.RetainedRetrieval, http.MethodGet, false)
}

func (s surface) cancelBatch(w http.ResponseWriter, r *http.Request) {
	localID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || action != "cancel" || localID == "" {
		x, _, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
		if !done {
			defer s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "Unknown Gemini batch action."))
		}
		return
	}
	r.SetPathValue("name", localID)
	s.relayBatch(w, r, kernel.RetainedHousekeeping, http.MethodPost, true)
}

func (s surface) deleteBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, resource *resources.Resource, p *kernel.Pin) *fault.Error {
		if err := s.kernel.RequireBatchEvidenceSettled(ctx, resource.UUID); err != nil {
			return err
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "batches/"+url.PathEscape(resource.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil && failure.Status != http.StatusNotFound {
			return kernel.UpstreamError(failure)
		}
		if response != nil {
			_ = response.Body.Close()
			response.Exchange.Complete(kernel.ClassSuccess)
		}
		if err := s.store.Tombstone(ctx, resource.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch mapping could not be deleted.")
		}

		if response == nil {
			s.kernel.WriteStateJSON(w, x, []byte(`{}`))
			return nil
		}
		s.kernel.WriteStateJSON(w, x, []byte(`{}`), response.Exchange)
		return nil
	})
}

func batchPageCursor(query url.Values) (int, string, *fault.Error) {
	pageSize := 10
	if raw := query.Get("pageSize"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			param := "pageSize"
			return 0, "", fault.InvalidRequest("invalid_value", "pageSize must be an integer between 1 and 100.", &param)
		}
		pageSize = parsed
	}
	token := query.Get("pageToken")
	if token != "" && !strings.HasPrefix(token, kernel.RetainedListCursorPrefix) && !validResourceID(token, resources.KindGeminiBatch) {
		param := "pageToken"
		return 0, "", fault.InvalidRequest("invalid_value", "pageToken must be a local Gemini batch identifier.", &param)
	}
	return pageSize, token, nil
}

func validResourceID(value, kind string) bool {
	prefix := kind + "_"
	raw := strings.TrimPrefix(value, prefix)
	if raw == value || len(raw) != 32 {
		return false
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	return err == nil && resources.LocalID(kind, id) == value
}
