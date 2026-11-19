package attachment

import (
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type API struct {
	server  *access.Server
	publish guardrails.Publisher
}

func NewAPI(server *access.Server, publish guardrails.Publisher) *API {
	return &API{server: server, publish: publish}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/guardrail-attachments", a.list)
	a.server.Route(mux, "POST /api/v1/guardrail-attachments", a.attach)
	a.server.Route(mux, "PUT /api/v1/guardrail-attachments/{attachment_id}", a.replace)
	a.server.Route(mux, "DELETE /api/v1/guardrail-attachments/{attachment_id}", a.detach)
}

func (a *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	items, err := list(r.Context(), a.server.Pool)
	if err != nil {
		return httpkit.Reply{}, err
	}
	visible := slices.DeleteFunc(items, func(item listed) bool { return authorize(p, item.target(), authz.View) != nil })
	return httpkit.Reply{Status: http.StatusOK, Body: map[string][]listed{"items": visible}}, nil
}

func (a *API) attach(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in guardrails.Attachment
	if err := httpkit.DecodeUnique(r, &in, 65536); err != nil {
		return httpkit.Reply{}, err
	}
	placement, err := parsePlacement(in)
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, placement, func(tx pgx.Tx, p authz.Principal) (target, error) {
		t, err := locate(ctx, tx, placement)
		if err != nil {
			return target{}, err
		}
		return t, authorize(p, t, authz.Change)
	}, func(tx pgx.Tx, p authz.Principal, t target) (httpkit.Reply, error) {
		if err := requirePolicy(ctx, tx, p, t); err != nil {
			return httpkit.Reply{}, err
		}
		created, err := create(ctx, tx, p.UserID(), placement)
		if err != nil {
			return httpkit.Reply{}, err
		}
		if err := a.settle(r, tx, p, "guardrail.attach", created.ID, "The effective policy is incompatible with an attached route."); err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Reply{Status: http.StatusCreated, Body: created}, nil
	})
}

func (a *API) replace(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "attachment_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var in struct {
		PolicyRevisionID string `json:"policy_revision_id"`
	}
	if err := httpkit.DecodeUnique(r, &in, 65536); err != nil {
		return httpkit.Reply{}, err
	}
	revisionID, err := httpkit.ParseUUID(in.PolicyRevisionID)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("policy_revision_id", "Choose a policy revision.")
	}
	ctx := r.Context()
	type locked struct {
		target
		etag string
	}
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (locked, error) {
		t, etag, err := lock(ctx, tx, id)
		if err != nil {
			return locked{}, err
		}
		return locked{t, etag}, authorize(p, t, authz.Change)
	}, func(tx pgx.Tx, p authz.Principal, current locked) (httpkit.Reply, error) {
		if err := httpkit.Match(r, current.etag); err != nil {
			return httpkit.Reply{}, err
		}
		next := current.target
		next.PolicyRevisionID = revisionID
		if err := requirePolicy(ctx, tx, p, next); err != nil {
			return httpkit.Reply{}, err
		}
		etag, err := repoint(ctx, tx, id, revisionID)
		if err != nil {
			return httpkit.Reply{}, err
		}
		if err := a.settle(r, tx, p, "guardrail.attachment.replace", id, "Activate the policy revision and check compatibility with attached routes."); err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Detail(struct {
			guardrails.Attachment
			ETag string `json:"etag"`
		}{next.Attachment, etag}, etag), nil
	})
}

func (a *API) detach(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "attachment_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	return access.Remove(a.server, r, id, func(tx pgx.Tx, p authz.Principal) (string, error) {
		t, etag, err := lock(ctx, tx, id)
		if err != nil {
			return "", err
		}
		return etag, authorize(p, t, authz.Change)
	}, func(tx pgx.Tx, p authz.Principal, etag string) (httpkit.Reply, error) {
		if err := httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, err
		}
		if err := remove(ctx, tx, id); err != nil {
			return httpkit.Reply{}, err
		}
		if err := a.settle(r, tx, p, "guardrail.detach", id, "The effective policy is incompatible with an attached route."); err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent}, nil
	})
}

func (a *API) settle(r *http.Request, tx pgx.Tx, p authz.Principal, action, id, incompatible string) error {
	if err := a.publish(r.Context(), tx, p.UserID()); err != nil {
		return httpkit.Invalid("attachment", incompatible)
	}
	return access.Audit(r.Context(), tx, r, p.Actor(), action, "guardrail_attachment", id, "success")
}
