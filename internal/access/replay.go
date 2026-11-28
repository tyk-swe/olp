package access

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type replayClaim struct {
	Key         string
	Fingerprint []byte
	Actor       string
}

// Call only after current authorization, inside the feature's mutation
// transaction. The actor, operation, target, precondition, and canonical input
// bind a replay; its encrypted response lives for at most 24 hours.
func (s *Server) replay(r *http.Request, tx pgx.Tx, p authz.Principal, input any) (replayClaim, *httpkit.Reply, error) {
	c := replayClaim{Key: r.Header.Get("Idempotency-Key"), Actor: p.ID}
	if len(c.Key) < 1 || len(c.Key) > 200 {
		return c, nil, problem.Fail(400, "idempotency_key_required", "Send an Idempotency-Key of 1–200 characters.")
	}
	data, err := json.Marshal([]any{r.Method, r.URL.Path, r.Header.Get("If-Match"), input})
	if err != nil {
		return c, nil, err
	}
	c.Fingerprint = s.Auth.Digest(secrets.MutationDigest, string(data))
	if err = secrets.DeleteExpired(r.Context(), tx, 100); err != nil {
		return c, nil, err
	}
	var stored []byte
	var id string
	err = tx.QueryRow(r.Context(), "SELECT fingerprint,secret_id::text FROM olp.replays WHERE actor=$1 AND key=$2 AND expires_at>now()", p.ID, c.Key).Scan(&stored, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, err
	}
	if !hmac.Equal(stored, c.Fingerprint) {
		return c, nil, problem.Fail(409, "idempotency_conflict", "This Idempotency-Key belongs to a different request.")
	}
	data, err = s.Keys.Read(r.Context(), tx, s.Installation, id, secrets.MutationReplay)
	if err != nil {
		return c, nil, err
	}
	// The encrypted reply already contains its authoritative JSON. Decoding
	// Body into any would round native integers/decimals and underflow exponents
	// before the response writer serializes a replay.
	var response struct {
		Status         int                     `json:"status"`
		Body           json.RawMessage         `json:"body"`
		ETag           string                  `json:"etag,omitempty"`
		Location       string                  `json:"location,omitempty"`
		ParentMutation *httpkit.ParentMutation `json:"parent_mutation,omitempty"`
	}
	if err = json.Unmarshal(data, &response); err != nil {
		return c, nil, err
	}
	result := httpkit.Reply{Status: response.Status, ETag: response.ETag, Location: response.Location, ParentMutation: response.ParentMutation}
	if len(response.Body) > 0 && string(response.Body) != "null" {
		result.Body = response.Body
	}
	return c, &result, nil
}
func (s *Server) completeReplay(r *http.Request, tx pgx.Tx, c replayClaim, result httpkit.Reply) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	// Feature request and collection limits bound replies. Aggregated responses
	// (such as a 64-slot credential pool) can exceed a single request's byte limit
	// and must remain replayable in full for an otherwise valid write to commit.
	id := ids.New()
	expires := time.Now().Add(24 * time.Hour)
	if err = s.Keys.Store(r.Context(), tx, s.Installation, id, secrets.MutationReplay, data, &expires); err != nil {
		return err
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO olp.replays(actor,key,fingerprint,secret_id,expires_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(actor,key) DO UPDATE SET fingerprint=excluded.fingerprint,secret_id=excluded.secret_id,expires_at=excluded.expires_at", c.Actor, c.Key, c.Fingerprint, id, expires)
	return err
}

const purgeReplaysSQL = `WITH expired AS (
      SELECT ctid FROM olp.replays WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.replays target USING expired WHERE target.ctid = expired.ctid`

func PurgeExpiredReplays(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	tag, err := tx.Exec(ctx, purgeReplaysSQL, before, limit)
	return tag.RowsAffected(), err
}
