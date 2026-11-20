package openaistate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) batchCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	kind := kernel.DurableKind(resources.KindBatch, r.PathValue("id"))
	if kind == resources.KindStrictBatch && !authority.Policy.AllowProviderState {
		s.kernel.Refuse(x, w, kernel.ProviderStateForbidden())
		return
	}
	res, contract, err := s.kernel.ReadDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No batch with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."))
		return
	}
	p, route, e := s.kernel.AdmitDurable(r.Context(), x, authority, res, contract, "batch", use)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
	}
}

func (s surface) listBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	limit, after, e := kernel.ListArgs(r)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	kinds := []string{resources.KindBatch}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictBatch)
	}
	// One extra row reports whether another page follows; it is never
	// authorized or projected.
	rows, err := s.kernel.Deps.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.kernel.Refuse(x, w, kernel.ResourceListError(err, "The batch list could not be read."))
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Kind == resources.KindStrictBatch {
			_, contract, readErr := s.kernel.ReadDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.kernel.AuthorizeDurable(r.Context(), x, authority, row, contract, "batch", kernel.RetainedHousekeeping) != nil {
				s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list is unavailable."))
				return
			}
		}
		obj, err := s.kernel.BatchObject(r.Context(), row)
		if err != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list could not be projected."))
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.kernel.WriteStateJSON(w, x, kernel.ListBody(items, ids, hasMore))
}

func (s surface) getBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		return s.batchRefresh(ctx, x, res, p, adapter, http.MethodGet, upstream.JobsPath(adapter, res.UpstreamID, false), nil, w)
	})
}

func (s surface) cancelBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			if e := s.cancelGeminiManagedBatch(ctx, x, res, p); e != nil {
				return e
			}
			return s.batchRefresh(ctx, x, res, p, adapter, http.MethodGet, upstream.JobsPath(adapter, res.UpstreamID, false), nil, w)
		}
		if res.Kind == resources.KindStrictBatch {
			switch wire.BatchStageOf(res.State) {
			case wire.BatchTerminal:
				return fault.ServerError(http.StatusConflict, "resource_transition", "This batch has already reached a terminal state.")
			case wire.BatchCancelling:
				out, err := s.kernel.BatchObject(ctx, res)
				if err != nil {
					return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The accepted cancellation could not be projected.")
				}
				s.kernel.WriteStateJSON(w, x, out)
				return nil
			}
		}
		return s.batchRefresh(ctx, x, res, p, adapter, http.MethodPost, upstream.JobsPath(adapter, res.UpstreamID, true), []byte(`{}`), w)
	})
}

func storedAdapter(res *resources.Resource) (connectors.BatchAdapter, *fault.Error) {
	adapter, err := kernel.ResourceAdapter(res)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored provider mapping is malformed.")
	}
	return adapter, nil
}

func (s surface) cancelGeminiManagedBatch(ctx context.Context, x *kernel.Call, res *resources.Resource, p *kernel.Pin) *fault.Error {
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, upstream.JobsPath(connectors.BatchGeminiManaged, res.UpstreamID, true), nil)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	response, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodPost, endpoint, []byte(`{}`), "application/json")
	if failure != nil {
		return kernel.UpstreamError(failure)
	}
	_ = response.Body.Close()

	response.Exchange.Complete(kernel.ClassSuccess)
	return nil
}

type refreshedBatch struct {
	state    string
	metadata json.RawMessage
}

func (s surface) batchRefresh(ctx context.Context, x *kernel.Call, res *resources.Resource, p *kernel.Pin, adapter connectors.BatchAdapter, method, path string, body []byte, w http.ResponseWriter) *fault.Error {
	result, exchange, e := s.fetchBatch(ctx, x, p, method, path, body)
	if e != nil {
		return e
	}
	refreshed, e := refreshBatchMetadata(res, adapter, result, s.kernel.Config.MaxResponseBytes)
	if e != nil {
		return e
	}
	commitCtx, stopCommit := ctx, func() {}
	if method == http.MethodPost {
		commitCtx, stopCommit = kernel.ResourceCommitContext(ctx)
	}
	defer stopCommit()
	current := *res
	if res.Kind == resources.KindStrictBatch {
		if e := s.commitStrictBatchRefresh(ctx, commitCtx, exchange, res, refreshed.state, result); e != nil {
			return e
		}
	} else {
		exchange.Complete(kernel.ClassSuccess)
		if err := s.kernel.Deps.Resources.Update(commitCtx, res.ID, refreshed.state, refreshed.metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed batch state could not be committed.")
		}
		current.State, current.Metadata = refreshed.state, refreshed.metadata
	}
	out, err := s.kernel.BatchObject(ctx, &current)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
	}
	s.kernel.WriteStateJSON(w, x, out, exchange)
	return nil
}

func (s surface) fetchBatch(ctx context.Context, x *kernel.Call, p *kernel.Pin, method, path string, body []byte) ([]byte, *kernel.PinnedExchange, *fault.Error) {
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, path, nil)
	if err != nil {
		return nil, nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.kernel.PinnedDo(ctx, x, p, method, endpoint, body, "application/json")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			return nil, nil, fault.NotFoundError("not_found", "No batch with this identifier exists for this key.")
		}
		return nil, nil, kernel.UpstreamError(failure)
	}
	defer resp.Body.Close()

	result, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
	if err != nil {
		return nil, nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	return result, resp.Exchange, nil
}

func refreshBatchMetadata(res *resources.Resource, adapter connectors.BatchAdapter, result []byte, maxBytes int64) (refreshedBatch, *fault.Error) {
	malformed := fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
	record, err := kernel.DecodeBatchRecord(res)
	if err != nil {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored batch mapping is malformed.")
	}
	switch {
	case adapter == connectors.BatchGeminiManaged:
		return refreshGeminiManagedBatch(res, result)
	case adapter.Managed():
		projected, upstreamID, err := upstream.ProjectManagedBatch(adapter, result, cmp.Or(record.CompletionWindow, "24h"))
		if err != nil {
			return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unknown batch status.")
		}
		if upstreamID != res.UpstreamID {
			return refreshedBatch{}, malformed
		}
		adoption, err := upstream.FilesMetadata(adapter, projected, kernel.ResourceModel(res), record.LocalInputFileID, record.Metadata)
		if err != nil {
			return refreshedBatch{}, malformed
		}
		return refreshedBatch{state: adoption.State, metadata: adoption.Metadata}, nil
	default:
		if res.Kind != resources.KindStrictBatch && !kernel.RetainedIdentityMatches(result, res.UpstreamID, maxBytes) {
			return refreshedBatch{}, malformed
		}
		adoption, err := upstream.FilesMetadata(connectors.BatchOpenAI, result, kernel.ResourceModel(res), record.LocalInputFileID, nil)
		if err != nil {
			return refreshedBatch{}, malformed
		}
		return refreshedBatch{state: adoption.State, metadata: adoption.Metadata}, nil
	}
}

func refreshGeminiManagedBatch(res *resources.Resource, result []byte) (refreshedBatch, *fault.Error) {
	batch, err := upstream.DecodeGeminiBatch(result)
	if err != nil || batch.ID != res.UpstreamID {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
	}
	base, err := kernel.DecodeGeminiManagedRecord(res.Metadata)
	if err != nil {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored Gemini batch could not be projected.")
	}
	managed, err := kernel.GeminiManagedBatchMetadata(result, kernel.ResourceModel(res), base)
	if errors.Is(err, kernel.ErrGeminiManagedBatchState) {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unknown Gemini batch status.")
	}
	if err != nil {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a malformed Gemini batch object.")
	}
	return refreshedBatch{state: managed.Status, metadata: managed.Metadata}, nil
}

func (s surface) commitStrictBatchRefresh(ctx, commitCtx context.Context, exchange *kernel.PinnedExchange, res *resources.Resource, state string, result []byte) *fault.Error {
	validated, _, err := kernel.StrictBatchResult(result, res.UpstreamID)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native batch result.")
	}
	_, contract, err := s.kernel.ReadDurable(ctx, res.Kind, res.APIKeyID, res.ID)
	if err != nil {
		exchange.Interrupt(kernel.ClassProtocol, true)
		return kernel.DurableError(err)
	}
	if !kernel.StrictBatchInputMatches(validated, contract.Effective) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider changed the batch input file identity.")
	}
	exchange.Complete(kernel.ClassSuccess)
	contract.Result = result
	payload, err := json.Marshal(contract)
	if err != nil {
		return kernel.DurableError(err)
	}
	if _, _, err = s.kernel.Deps.Resources.UpdateDurableContract(commitCtx, res.Kind, res.APIKeyID, res.ID, state, payload); err != nil {
		return kernel.DurableError(err)
	}
	return nil
}
