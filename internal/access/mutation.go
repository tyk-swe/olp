package access

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

// Event is the audit record of one committed change. Failed records an
// attempt that changed nothing but is still worth a row, such as a failed
// provider validation. The zero Event records nothing.
type Event struct {
	Action, Resource, ID string
	Project              *string
	Failed               bool
}

// Apply makes the change inside the mutation transaction and names its audit
// record, which the mutation writes before it commits.
type Apply[T any] func(pgx.Tx, authz.Principal, T) (httpkit.Reply, Event, error)

func (s *Server) record(r *http.Request, tx pgx.Tx, p authz.Principal, e Event) error {
	if e == (Event{}) {
		return nil
	}
	outcome := "success"
	if e.Failed {
		outcome = "failure"
	}
	return auditForProject(r.Context(), tx, r, p.Actor(), e.Action, e.Resource, e.ID, outcome, e.Project)
}

// Unlocked is the lock step of a change that loads and authorizes nothing
// before it applies, such as a create the route requirement already admits.
func Unlocked(pgx.Tx, authz.Principal) (struct{}, error) { return struct{}{}, nil }

// Mutate runs a change that carries an Idempotency-Key: lock loads and
// authorizes the current state, a completed request replays its stored reply,
// and otherwise apply makes the change.
func Mutate[T any](s *Server, r *http.Request, input any, lock func(pgx.Tx, authz.Principal) (T, error), apply Apply[T]) (httpkit.Reply, error) {
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := lock(tx, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	reply, err := applied(s, r, tx, p, current, apply)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, reply); err != nil {
		return httpkit.Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}

// Remove replays before locking because a completed removal leaves nothing to lock.
func Remove[T any](s *Server, r *http.Request, input any, lock func(pgx.Tx, authz.Principal) (T, error), apply Apply[T]) (httpkit.Reply, error) {
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	current, err := lock(tx, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := applied(s, r, tx, p, current, apply)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, reply); err != nil {
		return httpkit.Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}

// Transact is Mutate for a change that carries no Idempotency-Key.
func Transact[T any](s *Server, r *http.Request, lock func(pgx.Tx, authz.Principal) (T, error), apply Apply[T]) (httpkit.Reply, error) {
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := lock(tx, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := applied(s, r, tx, p, current, apply)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}

func applied[T any](s *Server, r *http.Request, tx pgx.Tx, p authz.Principal, current T, apply Apply[T]) (httpkit.Reply, error) {
	reply, event, err := apply(tx, p, current)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return reply, s.record(r, tx, p, event)
}
