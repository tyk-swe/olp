package apikeys

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

func pauseDecision(key storedKey) error {
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
		if err := pauseDecision(key); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		generation, err := access.AdvanceAuthority(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag := httpkit.NewID()
		var pausedAt time.Time
		if err := tx.QueryRow(r.Context(), "UPDATE olp.api_keys SET paused_at=now(),paused_authority_sequence=$1,etag=$2 WHERE id=$3 RETURNING paused_at", generation.Sequence, etag, id).Scan(&pausedAt); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body := map[string]any{"etag": etag, "runtime_generation": generation, "paused": true, "paused_at": pausedAt}
		return httpkit.Detail(body, etag), keyEvent("api_key.pause", id), nil
	})
}

func (a *API) resumeAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
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
		if err := resumeDecision(key, time.Now()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		generation, err := access.AdvanceAuthority(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag := httpkit.NewID()
		if _, err := tx.Exec(r.Context(), "UPDATE olp.api_keys SET paused_at=NULL,paused_authority_sequence=NULL,etag=$1 WHERE id=$2", etag, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body := map[string]any{"etag": etag, "runtime_generation": generation, "paused": false, "paused_at": nil}
		return httpkit.Detail(body, etag), keyEvent("api_key.resume", id), nil
	})
}

type pauseReplica struct {
	Instance          string `json:"gateway_instance"`
	AuthoritySequence *int64 `json:"authority_sequence"`
}

func pauseEnforced(paused bool, pauseSeq *int64, replicas []pauseReplica) bool {
	if !paused || pauseSeq == nil || len(replicas) == 0 {
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
	defer rows.Close()
	replicas := []pauseReplica{}
	for rows.Next() {
		var replica pauseReplica
		var sequence int64
		if err := rows.Scan(&replica.Instance, &sequence); err != nil {
			return httpkit.Reply{}, err
		}
		replica.AuthoritySequence = &sequence
		replicas = append(replicas, replica)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	paused := pausedAt != nil
	body := map[string]any{
		"paused":                    paused,
		"paused_at":                 pausedAt,
		"paused_authority_sequence": pauseSeq,
		"enforced":                  pauseEnforced(paused, pauseSeq, replicas),
		"replicas":                  replicas,
	}
	return httpkit.Detail(body, etag), nil
}
