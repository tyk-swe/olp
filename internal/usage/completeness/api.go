package completeness

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/param"
)

type API struct {
	Access *access.Server
}

func (s *API) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/request-metadata/gateway-epochs", s.listGatewayEpochs)
	s.Access.Route(mux, "POST /api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge",
		s.acknowledgeGatewayEpoch)
}

func (s *API) listGatewayEpochs(r *http.Request, p access.Principal) (access.Reply, error) {
	var err error
	query := r.URL.Query()
	limit, err := param.Limit(query)
	if err != nil {
		return access.Reply{}, err
	}
	cursor, err := param.Cursor(query)
	if err != nil {
		return access.Reply{}, err
	}
	state, err := epochStateParam(query)
	if err != nil {
		return access.Reply{}, err
	}
	items, next, err := ListGatewayEpochs(r.Context(), s.Access.Pool, state, cursor, limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(param.Page{Items: items, NextCursor: next}), nil
}

func epochStateParam(query url.Values) (*EpochState, error) {
	raw := param.Text(query, "state")
	if raw == nil {
		return nil, nil
	}
	state, known := ParseEpochState(*raw)
	if !known {
		return nil, access.Fail(400, "invalid_state", "Use open, gracefully_closed, unresolved, or acknowledged.")
	}
	return &state, nil
}

func (s *API) acknowledgeGatewayEpoch(r *http.Request, _ access.Principal) (access.Reply, error) {
	epoch, err := access.IDParam(r, "process_epoch")
	if err != nil {
		return access.Reply{}, err
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	acknowledgement, err := AcknowledgeGatewayEpoch(r.Context(), tx, epoch, principal.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	if acknowledgement == nil {
		return access.Reply{}, access.Fail(404, "not_found",
			"No unresolved gateway epoch with that identifier was found.")
	}
	// Acknowledging is an operator statement about loss they have seen, so each
	// acknowledgement is audited even when the epoch was already acknowledged.
	if err = access.Audit(r.Context(), tx, r, principal.Actor(),
		"request_metadata.gateway_epoch_acknowledge", "request_metadata_gateway_epoch",
		epoch, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.OK(acknowledgement), tx.Commit(r.Context())
}
