package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s *Server) GetPinnedFile(ctx context.Context, w http.ResponseWriter, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin) *fault.Error {
	adapter, adapterErr := ResourceAdapter(res)
	if adapterErr != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored provider mapping is malformed.")
	}
	path, e := ProviderFilePath(res, adapter)
	if e != nil {
		return e
	}
	endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, path, nil)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			if err := s.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
				return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
			}
			return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		return UpstreamError(failure)
	}
	defer resp.Body.Close()

	body, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	if e := s.verifyProviderFile(res, adapter, body); e != nil {
		return e
	}
	refreshed, err := RefreshedFileMetadata(res, body, adapter)
	if errors.Is(err, ErrProviderFileMetadata) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed file metadata.")
	}
	if err != nil {
		resp.Exchange.Interrupt(ClassProtocol, true)
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored file mapping is malformed.")
	}
	var out []byte
	if res.Kind == resources.KindStrictFile {
		if _, _, err := StrictResult(body, res.UpstreamID); err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
		}
		resp.Exchange.CompleteRetainedControl()
		_, contract, readErr := s.ReadDurable(ctx, res.Kind, authority.ID, res.ID)
		if readErr != nil {
			return DurableError(readErr)
		}
		contract.Result = body
		payload, marshalErr := json.Marshal(contract)
		if marshalErr != nil {
			return DurableError(marshalErr)
		}
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		defer stopCommit()
		if _, _, commitErr := s.Deps.Resources.UpdateDurableContract(commitCtx, res.Kind, authority.ID, res.ID, refreshed.State, payload); commitErr != nil {
			return DurableError(commitErr)
		}
		out, err = DurableProjection(body, res.ID, nil)
	} else {
		resp.Exchange.CompleteRetainedControl()
		if err := s.Deps.Resources.Update(ctx, res.ID, refreshed.State, refreshed.Metadata, refreshed.Expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
		}
		if adapter.Managed() {
			current := *res
			current.State, current.Metadata, current.ExpiresAt = refreshed.State, refreshed.Metadata, refreshed.Expires
			out, err = s.FileObject(ctx, &current)
		} else {
			out, err = RewriteID(body, "id", res.ID)
		}
	}
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	s.WriteStateJSON(w, x, out, resp.Exchange)
	return nil
}

func ProviderFilePath(res *resources.Resource, adapter connectors.BatchAdapter) (string, *fault.Error) {
	if adapter != connectors.BatchGeminiManaged {
		return "files/" + url.PathEscape(res.UpstreamID), nil
	}
	path, err := ManagedGeminiFilePath(res)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	return path, nil
}

func (s *Server) verifyProviderFile(res *resources.Resource, adapter connectors.BatchAdapter, body []byte) *fault.Error {
	if adapter == connectors.BatchGeminiManaged {
		var object struct {
			Name string `json:"name"`
		}
		name, err := GeminiManagedFileName(res)
		if err != nil || json.Unmarshal(body, &object) != nil || object.Name != name {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini file name.")
		}
		return nil
	}
	if res.Kind != resources.KindStrictFile && !RetainedIdentityMatches(body, res.UpstreamID, s.Config.MaxResponseBytes) {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	return nil
}
