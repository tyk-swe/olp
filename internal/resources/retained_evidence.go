package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

func persistRetainedEvidence(ctx context.Context, tx pgx.Tx, id uuid.UUID, key, digest, provenance, actor, previousState string, templates map[string]*usage.Event, observation RetainedObservation) (ingest.Persisted, error) {
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	documentObservation := observation
	documentObservation.ObservedAt = time.Time{}
	documentObservation.RemoteTerminated = false
	document, err := json.Marshal(documentObservation)
	if err != nil {
		return result, err
	}
	var priorDigest string
	var prior []byte
	err = tx.QueryRow(ctx, `SELECT digest,document FROM olp.retained_usage_evidence WHERE resource_id=$1 AND idempotency_key=$2`, id, key).Scan(&priorDigest, &prior)
	if err == nil {
		if priorDigest != digest || !sameJSON(prior, document) {
			return result, ErrEvidenceConflict
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		evidenceID := uuid.Must(uuid.NewV7())
		if _, err = tx.Exec(ctx, `INSERT INTO olp.retained_usage_evidence(id,resource_id,idempotency_key,digest,provenance,actor,document,remote_termination_verified,verified_terminal_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9)`, evidenceID, id, key, digest, provenance, actor, document, observation.RemoteTerminated, verifiedTerminalTime(observation)); err != nil {
			return result, err
		}
		for _, line := range observation.Lines {
			encoded, _ := json.Marshal(line.Usage)
			rows, e := tx.Query(ctx, `SELECT starts_at,ends_at,quantities FROM olp.retained_usage_coverage WHERE resource_id=$1 AND component=$2 AND model=$3 AND memory_tier=$4 AND starts_at<$6 AND ends_at>$5`, id, line.Component, line.Model, line.MemoryTier, line.Start, line.End)
			if e != nil {
				return result, e
			}
			duplicate := false
			for rows.Next() {
				var start, end time.Time
				var quantities []byte
				if e = rows.Scan(&start, &end, &quantities); e != nil {
					rows.Close()
					return result, e
				}
				if !start.Equal(line.Start) || !end.Equal(line.End) || !sameJSON(quantities, encoded) {
					rows.Close()
					return result, ErrEvidenceConflict
				}
				duplicate = true
			}
			rows.Close()
			if e = rows.Err(); e != nil {
				return result, e
			}
			if duplicate {
				continue
			}
			if previousState == "closed" {
				return result, ErrEvidenceConflict
			}
			template, _ := json.Marshal(templates[line.Model])
			event, e := usage.Decode(template)
			if e != nil {
				return result, e
			}
			identity := id.String() + ":" + line.Component + ":" + line.Model + ":" + line.MemoryTier + ":" + line.Start.Format(time.RFC3339Nano) + ":" + line.End.Format(time.RFC3339Nano)
			event.RequestID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
			event.EventID = event.RequestID
			event.ObservedAt = time.Now().UTC()
			event.RequestStartedAt = event.ObservedAt
			event.RequestCompletedAt = event.ObservedAt
			event.HTTPOutcomePending = false
			event.Attempts[0].ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity+":attempt")).String()
			// Keep the accepted pricing timestamp, including time-window tariffs.
			event.Attempts[0].CompletedAt = event.Attempts[0].StartedAt
			accepted, e := usage.NewAccountingEvent(event.WithFinalUsage(line.Usage))
			if e != nil {
				return result, e
			}
			persisted, e := ingest.PersistRetainedEventTx(ctx, tx, accepted)
			if e != nil {
				return result, e
			}
			if persisted.Outcome == ingest.PersistOutcomeRejectedOutsideReplayWindow {
				return result, ErrContract
			}
			result = persisted
			if _, e = tx.Exec(ctx, `INSERT INTO olp.retained_usage_coverage(resource_id,evidence_id,component,model,memory_tier,starts_at,ends_at,quantities,event_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, evidenceID, line.Component, line.Model, line.MemoryTier, line.Start, line.End, encoded, event.EventID); e != nil {
				return result, e
			}
		}
	}
	return result, nil
}
