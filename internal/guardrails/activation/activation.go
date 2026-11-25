package activation

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Publisher func(ctx context.Context, tx pgx.Tx, actor string) error

type Head struct {
	Project string
	ETag    string
	Retired bool
}

type Lineage interface {
	Lock(ctx context.Context, tx pgx.Tx, id string) (Head, error)
	Activate(ctx context.Context, tx pgx.Tx, actor, id, revisionID string) error
}

func Handler(server *access.Server, publish Publisher, lineage Lineage, param, resource string) func(*http.Request, authz.Principal) (httpkit.Reply, error) {
	return func(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
		id, err := httpkit.IDParam(r, param)
		if err != nil {
			return httpkit.Reply{}, err
		}
		var in struct {
			RevisionID string `json:"revision_id"`
		}
		if err := httpkit.DecodeUnique(r, &in, 65536); err != nil {
			return httpkit.Reply{}, err
		}
		ctx := r.Context()
		return access.Mutate(server, r, in, func(tx pgx.Tx, p authz.Principal) (Head, error) {
			head, err := lineage.Lock(ctx, tx, id)
			if err != nil {
				return Head{}, err
			}
			return head, p.Project(guardrails.ProjectPointer(head.Project), authz.Change)
		}, func(tx pgx.Tx, p authz.Principal, head Head) (httpkit.Reply, access.Event, error) {
			if err := httpkit.Match(r, head.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if head.Retired {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(http.StatusConflict, "guardrail_retired", "A retired resource cannot be activated.")
			}
			if err := lineage.Activate(ctx, tx, p.UserID(), id, in.RevisionID); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if err := publish(ctx, tx, p.UserID()); err != nil {
				return httpkit.Reply{}, access.Event{}, httpkit.Invalid("revision_id", "This activation is incompatible with a published route or attachment.")
			}
			return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "guardrail.activate", Resource: resource, ID: id}, nil
		})
	}
}

func Refused() error {
	return httpkit.Invalid("revision_id", "Choose a revision owned by this resource.")
}
