package apikeys

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

type pauseChange struct {
	paused bool
	event  string
	decide func(storedKey, time.Time) error
}

var (
	pauseKey  = pauseChange{paused: true, event: "api_key.pause", decide: pauseDecision}
	resumeKey = pauseChange{paused: false, event: "api_key.resume", decide: resumeDecision}
)

type pauseReply struct {
	ETag              string                     `json:"etag"`
	RuntimeGeneration access.AuthorityGeneration `json:"runtime_generation"`
	Paused            bool                       `json:"paused"`
	PausedAt          *time.Time                 `json:"paused_at"`
}

type pauseReplica struct {
	Instance          string `json:"gateway_instance"`
	AuthoritySequence *int64 `json:"authority_sequence"`
}

type pauseRolloutReply struct {
	Paused                  bool           `json:"paused"`
	PausedAt                *time.Time     `json:"paused_at"`
	PausedAuthoritySequence *int64         `json:"paused_authority_sequence"`
	Enforced                bool           `json:"enforced"`
	Replicas                []pauseReplica `json:"replicas"`
}

func pauseDecision(key storedKey, _ time.Time) error {
	if key.revoked != nil {
		return httpkit.Fail(409, "api_key_revoked", "A revoked key cannot be paused.")
	}
	if key.paused != nil {
		return httpkit.Fail(409, "api_key_paused", "This key is already paused.")
	}
	return nil
}

func resumeDecision(key storedKey, now time.Time) error {
	if key.revoked != nil {
		return httpkit.Fail(409, "api_key_revoked", "A revoked key cannot be resumed.")
	}
	if key.expires != nil && !key.expires.After(now) {
		return httpkit.Fail(409, "api_key_expired", "An expired key cannot be resumed.")
	}
	if key.paused == nil {
		return httpkit.Fail(409, "api_key_not_paused", "This key is not paused.")
	}
	return nil
}

func (a *API) pauseAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return a.changePause(r, pauseKey)
}

func (a *API) resumeAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return a.changePause(r, resumeKey)
}

func (a *API) changePause(r *http.Request, change pauseChange) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, nil, func(tx pgx.Tx, p authz.Principal) (storedKey, error) {
		return lockKey(r, tx, p, id)
	}, func(tx pgx.Tx, _ authz.Principal, key storedKey) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, key.etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := change.decide(key, time.Now()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		generation, err := access.AdvanceAuthority(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag := httpkit.NewID()
		var pausedAt *time.Time
		if err := tx.QueryRow(r.Context(), `UPDATE olp.api_keys SET paused_at=CASE WHEN $1 THEN now() END,
 paused_authority_sequence=CASE WHEN $1 THEN $2::bigint END,etag=$3 WHERE id=$4 RETURNING paused_at`, change.paused, generation.Sequence, etag, id).Scan(&pausedAt); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := pauseReply{ETag: etag, RuntimeGeneration: generation, Paused: change.paused, PausedAt: pausedAt}
		return httpkit.Detail(reply, etag), keyEvent(change.event, id), nil
	})
}

func pauseEnforced(pauseSeq *int64, replicas []pauseReplica) bool {
	if pauseSeq == nil || len(replicas) == 0 {
		return false
	}
	for _, replica := range replicas {
		if replica.AuthoritySequence == nil || *replica.AuthoritySequence < *pauseSeq {
			return false
		}
	}
	return true
}

func (a *API) pauseRollout(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var pausedAt *time.Time
	var pauseSeq *int64
	var projectID *string
	var etag string
	if err := a.server.Pool.QueryRow(r.Context(), "SELECT paused_at,paused_authority_sequence,project_id::text,etag::text FROM olp.api_keys WHERE id=$1", id).Scan(&pausedAt, &pauseSeq, &projectID, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(projectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), "SELECT gateway_instance,authority_sequence FROM olp.runtime_install_status WHERE checked_at>now()-make_interval(secs=>60) ORDER BY gateway_instance")
	if err != nil {
		return httpkit.Reply{}, err
	}
	replicas, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pauseReplica])
	if err != nil {
		return httpkit.Reply{}, err
	}
	paused := pausedAt != nil
	reply := pauseRolloutReply{Paused: paused, PausedAt: pausedAt, PausedAuthoritySequence: pauseSeq, Enforced: paused && pauseEnforced(pauseSeq, replicas), Replicas: replicas}
	return httpkit.Detail(reply, etag), nil
}
