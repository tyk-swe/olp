package completeness

import (
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
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

func (s *API) listGatewayEpochs(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var err error
	query := r.URL.Query()
	limit, err := param.Limit(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	cursor, err := param.Cursor(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	state, err := epochStateParam(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, next, err := ListGatewayEpochs(r.Context(), s.Access.Pool, state, cursor, limit)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(param.Page{Items: items, NextCursor: next}), nil
}

func epochStateParam(query url.Values) (*EpochState, error) {
	raw := param.Text(query, "state")
	if raw == nil {
		return nil, nil
	}
	state, known := ParseEpochState(*raw)
	if !known {
		return nil, problem.Fail(400, "invalid_state", "Use open, gracefully_closed, unresolved, or acknowledged.")
	}
	return &state, nil
}

func (s *API) acknowledgeGatewayEpoch(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	epoch, err := httpkit.IDParam(r, "process_epoch")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.Access, r, access.Unlocked, func(tx pgx.Tx, principal authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		acknowledgement, err := AcknowledgeGatewayEpoch(r.Context(), tx, epoch, principal.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if acknowledgement == nil {
			return httpkit.Reply{}, access.Event{}, problem.Fail(404, "not_found",
				"No unresolved gateway epoch with that identifier was found.")
		}
		// Acknowledging is an operator statement about loss they have seen, so each
		// acknowledgement is audited even when the epoch was already acknowledged.
		return httpkit.OK(acknowledgement), access.Event{Action: "request_metadata.gateway_epoch_acknowledge", Resource: "request_metadata_gateway_epoch", ID: epoch}, nil
	})
}
