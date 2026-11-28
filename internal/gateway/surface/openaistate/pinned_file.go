package openaistate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s surface) getPinnedFile(ctx context.Context, w http.ResponseWriter, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin) *fault.Error {
	adapter, adapterErr := managedbatch.ResourceAdapter(res)
	if adapterErr != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored provider mapping is malformed.")
	}
	path, e := providerFilePath(res, adapter)
	if e != nil {
		return e
	}
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, path, nil)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			if err := s.store.Tombstone(ctx, res.ID); err != nil {
				return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
			}
			return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		return kernel.UpstreamError(failure)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	if e := s.verifyProviderFile(res, adapter, body); e != nil {
		return e
	}
	refreshed, err := refreshedFileMetadata(res, body, adapter)
	if errors.Is(err, managedbatch.ErrProviderFileMetadata) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed file metadata.")
	}
	if err != nil {
		resp.Exchange.Interrupt(kernel.ClassProtocol, true)
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored file mapping is malformed.")
	}
	out, e := s.commitRefreshedFile(ctx, authority.ID, res, adapter, body, refreshed, resp.Exchange)
	if e != nil {
		return e
	}
	s.kernel.WriteStateJSON(w, x, out, resp.Exchange)
	return nil
}

func (s surface) commitRefreshedFile(ctx context.Context, owner string, res *resources.Resource, adapter connectors.BatchAdapter, body []byte, refreshed refreshedFile, exchange *kernel.PinnedExchange) ([]byte, *fault.Error) {
	if res.Kind == resources.KindStrictFile {
		return s.commitStrictFile(ctx, owner, res, body, refreshed.State, exchange)
	}
	exchange.CompleteRetainedControl()
	if err := s.store.Update(ctx, res.ID, refreshed.State, refreshed.Metadata, refreshed.Expires); err != nil {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
	}
	if !adapter.Managed() {
		return projectedFile(kernel.RewriteID(body, "id", res.ID))
	}
	current := *res
	current.State, current.Metadata, current.ExpiresAt = refreshed.State, refreshed.Metadata, refreshed.Expires
	return projectedFile(s.fileObject(ctx, &current))
}

func (s surface) commitStrictFile(ctx context.Context, owner string, res *resources.Resource, body []byte, state string, exchange *kernel.PinnedExchange) ([]byte, *fault.Error) {
	if _, _, err := strictResult(body, res.UpstreamID); err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
	}
	exchange.CompleteRetainedControl()
	_, contract, readErr := s.kernel.ReadDurable(ctx, res.Kind, owner, res.ID)
	if readErr != nil {
		return nil, kernel.DurableError(readErr)
	}
	contract.Result = body
	payload, marshalErr := json.Marshal(contract)
	if marshalErr != nil {
		return nil, kernel.DurableError(marshalErr)
	}
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	defer stopCommit()
	if _, _, commitErr := s.store.UpdateDurableContract(commitCtx, res.Kind, owner, res.ID, state, payload); commitErr != nil {
		return nil, kernel.DurableError(commitErr)
	}
	return projectedFile(durableProjection(body, res.ID, nil))
}

func projectedFile(out []byte, err error) ([]byte, *fault.Error) {
	if err != nil {
		return nil, malformedFileObject()
	}
	return out, nil
}

func providerFilePath(res *resources.Resource, adapter connectors.BatchAdapter) (string, *fault.Error) {
	if adapter != connectors.BatchGeminiManaged {
		return "files/" + url.PathEscape(res.UpstreamID), nil
	}
	path, err := managedGeminiFilePath(res)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	return path, nil
}

func (s surface) verifyProviderFile(res *resources.Resource, adapter connectors.BatchAdapter, body []byte) *fault.Error {
	if adapter == connectors.BatchGeminiManaged {
		var object struct {
			Name string `json:"name"`
		}
		name, err := geminiManagedFileName(res)
		if err != nil || json.Unmarshal(body, &object) != nil || object.Name != name {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini file name.")
		}
		return nil
	}
	if res.Kind != resources.KindStrictFile && !retainedIdentityMatches(body, res.UpstreamID, s.kernel.Config.MaxResponseBytes) {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	return nil
}
