package attachment

import (
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
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

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	items, err := list(r.Context(), a.server.Pool)
	if err != nil {
		return access.Reply{}, err
	}
	visible := slices.DeleteFunc(items, func(item listed) bool { return authorize(p, item.target(), access.View) != nil })
	return access.Reply{Status: http.StatusOK, Body: map[string][]listed{"items": visible}}, nil
}

func (a *API) attach(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in guardrails.Attachment
	if err := access.DecodeUnique(r, &in, 65536); err != nil {
		return access.Reply{}, err
	}
	placement, err := parsePlacement(in)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, placement, func(tx pgx.Tx, p access.Principal) (target, error) {
		t, err := locate(ctx, tx, placement)
		if err != nil {
			return target{}, err
		}
		return t, authorize(p, t, access.Change)
	}, func(tx pgx.Tx, p access.Principal, t target) (access.Reply, error) {
		if err := requirePolicy(ctx, tx, p, t); err != nil {
			return access.Reply{}, err
		}
		created, err := create(ctx, tx, p.UserID(), placement)
		if err != nil {
			return access.Reply{}, err
		}
		if err := a.settle(r, tx, p, "guardrail.attach", created.ID, "The effective policy is incompatible with an attached route."); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusCreated, Body: created}, nil
	})
}

func (a *API) replace(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "attachment_id")
	if err != nil {
		return access.Reply{}, err
	}
	var in struct {
		PolicyRevisionID string `json:"policy_revision_id"`
	}
	if err := access.DecodeUnique(r, &in, 65536); err != nil {
		return access.Reply{}, err
	}
	revisionID, err := access.ParseUUID(in.PolicyRevisionID)
	if err != nil {
		return access.Reply{}, access.Invalid("policy_revision_id", "Choose a policy revision.")
	}
	ctx := r.Context()
	type locked struct {
		target
		etag string
	}
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p access.Principal) (locked, error) {
		t, etag, err := lock(ctx, tx, id)
		if err != nil {
			return locked{}, err
		}
		return locked{t, etag}, authorize(p, t, access.Change)
	}, func(tx pgx.Tx, p access.Principal, current locked) (access.Reply, error) {
		if err := access.Match(r, current.etag); err != nil {
			return access.Reply{}, err
		}
		next := current.target
		next.PolicyRevisionID = revisionID
		if err := requirePolicy(ctx, tx, p, next); err != nil {
			return access.Reply{}, err
		}
		etag, err := repoint(ctx, tx, id, revisionID)
		if err != nil {
			return access.Reply{}, err
		}
		if err := a.settle(r, tx, p, "guardrail.attachment.replace", id, "Activate the policy revision and check compatibility with attached routes."); err != nil {
			return access.Reply{}, err
		}
		return access.Detail(struct {
			guardrails.Attachment
			ETag string `json:"etag"`
		}{next.Attachment, etag}, etag), nil
	})
}

func (a *API) detach(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "attachment_id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Remove(a.server, r, id, func(tx pgx.Tx, p access.Principal) (string, error) {
		t, etag, err := lock(ctx, tx, id)
		if err != nil {
			return "", err
		}
		return etag, authorize(p, t, access.Change)
	}, func(tx pgx.Tx, p access.Principal, etag string) (access.Reply, error) {
		if err := access.Match(r, etag); err != nil {
			return access.Reply{}, err
		}
		if err := remove(ctx, tx, id); err != nil {
			return access.Reply{}, err
		}
		if err := a.settle(r, tx, p, "guardrail.detach", id, "The effective policy is incompatible with an attached route."); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent}, nil
	})
}

func (a *API) settle(r *http.Request, tx pgx.Tx, p access.Principal, action, id, incompatible string) error {
	if err := a.publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Invalid("attachment", incompatible)
	}
	return access.Audit(r.Context(), tx, r, p.Actor(), action, "guardrail_attachment", id, "success")
}
