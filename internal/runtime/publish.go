package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

// Published identifies a recorded release.
type Published struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

// Publish compiles the serving snapshot from active provider revisions and
// published routes, validates it, and records it as the next release inside
// the caller's transaction. The caller holds the installation row lock, so
// sequences are gapless and publication is atomic with the activation that
// caused it. Republishing unchanged configuration is harmless: it records a
// new sequence with the same digest.
func Publish(ctx context.Context, tx pgx.Tx, actor string) (Published, error) {
	snapshot, err := Compile(ctx, tx)
	if err != nil {
		return Published{}, err
	}
	var sequence int64
	if err = tx.QueryRow(ctx, "UPDATE olp.installation SET release_sequence=release_sequence+1 WHERE singleton RETURNING release_sequence").Scan(&sequence); err != nil {
		return Published{}, err
	}
	now := time.Now().UTC()
	snapshot.Generation = Generation{ID: uuid.Must(uuid.NewV7()).String(), Ordinal: sequence, ActivatedAt: now}
	if err = snapshot.Validate(); err != nil {
		if refusal := StrictRefusal(err); refusal != nil {
			return Published{}, refusal
		}
		return Published{}, fmt.Errorf("release rejected: %w", err)
	}
	digest, err := snapshot.Digest()
	if err != nil {
		return Published{}, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return Published{}, err
	}
	p := Published{ID: snapshot.Generation.ID, Sequence: sequence}
	_, err = tx.Exec(ctx, "INSERT INTO olp.runtime_releases(id,sequence,sha256,snapshot,created_by,created_at,published_at) VALUES($1,$2,$3,$4,$5,$6,$6)", p.ID, sequence, digest, encoded, actor, now)
	return p, err
}

// StrictRefusal reports an obligation a strict route's target cannot meet as a
// 422, or returns nil when err is not such an obligation. A refusal that exists
// only because the target has no provider profile or needs translation tells
// the author to declare the route transformed.
func StrictRefusal(err error) error {
	var incompatible incompatibilityError
	if !errors.As(err, &incompatible) {
		return nil
	}
	code, _, requirement, message := incompatible.Incompatibility()
	switch requirement {
	case "explicit_profile":
		message += " Declare the route transformed to use a provider without a profile."
	case "strict_profile":
		message += " Declare the route transformed to use this profile."
	case "operation_contract", "native_media_contract", "native_batch_contract", "native_realtime_contract":
		message += " Declare the route transformed to translate for this target."
	}
	return access.Fail(422, code, message)
}

// Compile reads the serving configuration without recording a release. Draft
// state never appears here: providers serve their active revision and routes
// their latest published revision.
func Compile(ctx context.Context, tx pgx.Tx) (*Snapshot, error) {
	snapshot := &Snapshot{Providers: map[string]Provider{}, Routes: map[string]Route{}}
	rows, err := tx.Query(ctx, "SELECT p.id::text,p.state,r.id::text,r.name,r.configuration,r.models,r.slots,p.project_id::text,"+PluginColumn+" FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id WHERE p.state IN ('active','disabled')")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var revision ProviderRevision
		if err = rows.Scan(&revision.ID, &revision.State, &revision.RevisionID, &revision.Name, &revision.Configuration, &revision.Models, &revision.Slots, &revision.ProjectID, &revision.Plugin); err != nil {
			rows.Close()
			return nil, err
		}
		provider, err := DecodeProviderRevision(revision)
		if err != nil {
			rows.Close()
			return nil, err
		}
		provider.Limits = publishedLimits(provider.Limits)
		snapshot.Providers[provider.ID] = provider
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, "SELECT r.id::text,r.slug,v.id::text,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.content_policy,v.fidelity FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id WHERE r.state='active'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var revision RouteRevision
		if err = rows.Scan(&revision.ID, &revision.Slug, &revision.RevisionID, &revision.Revision, &revision.Operations, &revision.OverallTimeout, &revision.MaxAttempts, &revision.Targets, &revision.PublishedAt, &revision.Policy, &revision.ProjectID, &revision.ContentPolicy, &revision.Fidelity); err != nil {
			return nil, err
		}
		route, err := DecodeRouteRevision(revision)
		if err != nil {
			return nil, err
		}
		snapshot.Routes[route.Slug] = route
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, "SELECT scope,scope_id::text,policy FROM olp.routing_policies WHERE scope IN ('installation','api-key')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope, id string
		var raw []byte
		if err = rows.Scan(&scope, &id, &raw); err != nil {
			return nil, err
		}
		var p Policy
		if err = json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if scope == "installation" {
			snapshot.InstallationPolicy = &p
		} else {
			if snapshot.KeyPolicies == nil {
				snapshot.KeyPolicies = map[string]*Policy{}
			}
			snapshot.KeyPolicies[id] = &p
		}
	}
	return snapshot, rows.Err()
}

// publishedLimits drops a connection quota that bounds nothing so clearing
// every limit publishes the same snapshot as never setting one.
func publishedLimits(l *Limits) *Limits {
	if l == nil || (l.RequestsPerMinute == nil && l.TokensPerMinute == nil && l.MaxConcurrency == nil) {
		return nil
	}
	return l
}
