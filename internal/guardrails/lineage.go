package guardrails

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
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

func Activation(server *access.Server, publish Publisher, lineage Lineage, param, resource string) func(*http.Request, access.Principal) (access.Reply, error) {
	return func(r *http.Request, _ access.Principal) (access.Reply, error) {
		id, err := access.IDParam(r, param)
		if err != nil {
			return access.Reply{}, err
		}
		var in struct {
			RevisionID string `json:"revision_id"`
		}
		if err := access.DecodeUnique(r, &in, 65536); err != nil {
			return access.Reply{}, err
		}
		ctx := r.Context()
		return access.Mutate(server, r, in, func(tx pgx.Tx, p access.Principal) (Head, error) {
			head, err := lineage.Lock(ctx, tx, id)
			if err != nil {
				return Head{}, err
			}
			return head, p.Project(ProjectPointer(head.Project), access.Change)
		}, func(tx pgx.Tx, p access.Principal, head Head) (access.Reply, error) {
			if err := access.Match(r, head.ETag); err != nil {
				return access.Reply{}, err
			}
			if head.Retired {
				return access.Reply{}, access.Fail(http.StatusConflict, "guardrail_retired", "A retired resource cannot be activated.")
			}
			if err := lineage.Activate(ctx, tx, p.UserID(), id, in.RevisionID); err != nil {
				return access.Reply{}, err
			}
			if err := publish(ctx, tx, p.UserID()); err != nil {
				return access.Reply{}, access.Invalid("revision_id", "This activation is incompatible with a published route or attachment.")
			}
			if err := access.Audit(ctx, tx, r, p.Actor(), "guardrail.activate", resource, id, "success"); err != nil {
				return access.Reply{}, err
			}
			return access.Reply{Status: http.StatusNoContent}, nil
		})
	}
}

func ActivationRefused() error {
	return access.Invalid("revision_id", "Choose a revision owned by this resource.")
}
