package access

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReplayClaim struct {
	Key         string
	Fingerprint []byte
	Actor       string
}

// Call only after current authorization, inside the feature's mutation
// transaction. The actor, operation, target, precondition, and canonical input
// bind a replay; its encrypted response lives for at most 24 hours.
func (s *Server) Replay(r *http.Request, tx pgx.Tx, p Principal, input any) (ReplayClaim, *Reply, error) {
	c := ReplayClaim{Key: r.Header.Get("Idempotency-Key"), Actor: p.ID}
	if len(c.Key) < 1 || len(c.Key) > 200 {
		return c, nil, Fail(400, "idempotency_key_required", "Send an Idempotency-Key of 1–200 characters.")
	}
	data, err := json.Marshal([]any{r.Method, r.URL.Path, r.Header.Get("If-Match"), input})
	if err != nil {
		return c, nil, err
	}
	c.Fingerprint = s.Auth.Digest("mutation", string(data))
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.secrets WHERE id IN (SELECT id FROM olp_go.secrets WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return c, nil, err
	}
	var stored []byte
	var id string
	err = tx.QueryRow(r.Context(), "SELECT fingerprint,secret_id::text FROM olp_go.replays WHERE actor=$1 AND key=$2 AND expires_at>now()", p.ID, c.Key).Scan(&stored, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, err
	}
	if !hmac.Equal(stored, c.Fingerprint) {
		return c, nil, Fail(409, "idempotency_conflict", "This Idempotency-Key belongs to a different request.")
	}
	data, err = s.Keys.Read(r.Context(), tx, s.Installation, id, "mutation_replay")
	if err != nil {
		return c, nil, err
	}
	var response Reply
	if err = json.Unmarshal(data, &response); err != nil {
		return c, nil, err
	}
	return c, &response, nil
}
func (s *Server) CompleteReplay(r *http.Request, tx pgx.Tx, c ReplayClaim, result Reply) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	// Feature request and collection limits bound replies. Aggregated responses
	// (such as a 64-slot credential pool) can exceed a single request's byte limit
	// and must remain replayable in full for an otherwise valid write to commit.
	id := NewID()
	expires := time.Now().Add(24 * time.Hour)
	if err = s.Keys.Store(r.Context(), tx, s.Installation, id, "mutation_replay", data, &expires); err != nil {
		return err
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO olp_go.replays(actor,key,fingerprint,secret_id,expires_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(actor,key) DO UPDATE SET fingerprint=excluded.fingerprint,secret_id=excluded.secret_id,expires_at=excluded.expires_at", c.Actor, c.Key, c.Fingerprint, id, expires)
	return err
}
