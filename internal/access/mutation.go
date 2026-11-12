package access

import (
	"net/http"

	"github.com/jackc/pgx/v5"
)

func Mutate[T any](s *Server, r *http.Request, input any, lock func(pgx.Tx, Principal) (T, error), apply func(pgx.Tx, Principal, T) (Reply, error)) (Reply, error) {
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := lock(tx, p)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	return s.settle(r, tx, claim, func() (Reply, error) { return apply(tx, p, current) })
}

// Remove replays before locking because a completed removal leaves nothing to lock.
func Remove[T any](s *Server, r *http.Request, input any, lock func(pgx.Tx, Principal) (T, error), apply func(pgx.Tx, Principal, T) (Reply, error)) (Reply, error) {
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	current, err := lock(tx, p)
	if err != nil {
		return Reply{}, err
	}
	return s.settle(r, tx, claim, func() (Reply, error) { return apply(tx, p, current) })
}

// Transact is Mutate for a change that carries no Idempotency-Key.
func Transact[T any](s *Server, r *http.Request, lock func(pgx.Tx, Principal) (T, error), apply func(pgx.Tx, Principal, T) (Reply, error)) (Reply, error) {
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := lock(tx, p)
	if err != nil {
		return Reply{}, err
	}
	reply, err := apply(tx, p, current)
	if err != nil {
		return Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}

func (s *Server) settle(r *http.Request, tx pgx.Tx, claim ReplayClaim, apply func() (Reply, error)) (Reply, error) {
	reply, err := apply()
	if err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, reply); err != nil {
		return Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}
