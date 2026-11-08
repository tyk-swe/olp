package routes

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

type DraftSlot interface{ draftSlot() }

type NewDraft struct {
	ID        string
	CreatedBy string
	ProjectID *string
	BasedOn   *string
}

type ExistingDraft struct{ ID string }

func (NewDraft) draftSlot()      {}
func (ExistingDraft) draftSlot() {}

type DraftContent struct {
	Slug                 string
	Operations           []string
	OverallTimeoutMS     int
	MaxAttempts          int
	Targets              []routing.PublishedTarget
	ContentPolicy        json.RawMessage
	Fidelity             json.RawMessage
	Behavior             routing.Behavior
	ExposeUpstreamModels *bool
}

type OpenDraft struct {
	ID                   string
	Slug                 string
	ProjectID            *string
	Operations           []string
	OverallTimeoutMS     int
	MaxAttempts          int
	Targets              []routing.PublishedTarget
	ContentPolicy        json.RawMessage
	Fidelity             json.RawMessage
	Behavior             routing.Behavior
	ExposeUpstreamModels bool
	RoutingPolicy        *routing.Policy
}

type PublishedRoute struct {
	OpenDraft
	Retired bool
}

func (in *DraftInput) Content(targets []routing.PublishedTarget, exposeUpstreamModels *bool) DraftContent {
	return DraftContent{
		Slug: in.Slug, Operations: in.Operations, OverallTimeoutMS: in.OverallTimeoutMS, MaxAttempts: in.MaxAttempts, Targets: targets,
		ContentPolicy: in.ContentPolicy, Fidelity: in.Fidelity, Behavior: in.Behavior, ExposeUpstreamModels: exposeUpstreamModels,
	}
}

func StageDraft(ctx context.Context, tx pgx.Tx, slot DraftSlot, etag string, content DraftContent) error {
	operations, err := json.Marshal(content.Operations)
	if err != nil {
		return err
	}
	targets, err := json.Marshal(content.Targets)
	if err != nil {
		return err
	}
	behavior, err := json.Marshal(content.Behavior)
	if err != nil {
		return err
	}
	switch slot := slot.(type) {
	case NewDraft:
		_, err := tx.Exec(ctx, "INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity,behavior,catalog_expose_upstream_models,based_on_revision_id) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)", slot.ID, content.Slug, operations, content.OverallTimeoutMS, content.MaxAttempts, targets, content.ContentPolicy, etag, slot.CreatedBy, slot.ProjectID, content.Fidelity, behavior, content.ExposeUpstreamModels, slot.BasedOn)
		return err
	case ExistingDraft:
		_, err := tx.Exec(ctx, "UPDATE olp.route_drafts SET slug=$2,state='draft',operations=$3,overall_timeout_ms=$4,max_attempts=$5,targets=$6,content_policy=$7,etag=$8,fidelity=$9,behavior=$10,catalog_expose_upstream_models=COALESCE($11,catalog_expose_upstream_models),updated_at=now() WHERE id=$1", slot.ID, content.Slug, operations, content.OverallTimeoutMS, content.MaxAttempts, targets, content.ContentPolicy, etag, content.Fidelity, behavior, content.ExposeUpstreamModels)
		return err
	}
	return nil
}

func StageDraftPolicy(ctx context.Context, tx pgx.Tx, draftID, etag, actor string, policy *routing.Policy) error {
	if policy == nil {
		_, err := tx.Exec(ctx, "DELETE FROM olp.routing_policies WHERE scope='route-draft' AND scope_id=$1", draftID)
		return err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4) ON CONFLICT(scope,scope_id) DO UPDATE SET policy=excluded.policy,etag=excluded.etag,updated_by=excluded.updated_by,configured=true,updated_at=now()", draftID, encoded, etag, actor)
	return err
}

func ReadOpenDrafts(ctx context.Context, q access.Queryer) ([]OpenDraft, error) {
	rows, err := q.Query(ctx, `SELECT d.id::text,d.slug,d.project_id::text,d.operations,d.overall_timeout_ms,d.max_attempts,d.targets,d.content_policy,d.fidelity,d.behavior,
		COALESCE(d.catalog_expose_upstream_models,(SELECT c.expose_upstream_models FROM olp.model_catalog_route_settings c JOIN olp.routes r ON r.id=c.route_id WHERE r.slug=d.slug),false),p.policy,false
		FROM olp.route_drafts d LEFT JOIN olp.routing_policies p ON p.scope='route-draft' AND p.scope_id=d.id
		WHERE d.state IN ('draft','validated') AND d.based_on_revision_id IS NULL ORDER BY d.created_at,d.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenDraft, error) {
		draft, err := scanStoredRoute(row)
		return draft.OpenDraft, err
	})
}

func ReadPublishedRoutes(ctx context.Context, q access.Queryer) ([]PublishedRoute, error) {
	rows, err := q.Query(ctx, `SELECT r.id::text,r.slug,r.project_id::text,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.content_policy,v.fidelity,v.behavior,
		COALESCE((SELECT c.expose_upstream_models FROM olp.model_catalog_route_settings c WHERE c.route_id=r.id),false),v.routing_policy,r.state='retired'
		FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id ORDER BY r.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanStoredRoute)
}

func scanStoredRoute(row pgx.CollectableRow) (PublishedRoute, error) {
	var id, slug string
	var project *string
	var timeout, attempts int
	var expose, retired bool
	var operations, targets, contentPolicy, fidelity, behavior, policy []byte
	if err := row.Scan(&id, &slug, &project, &operations, &timeout, &attempts, &targets, &contentPolicy, &fidelity, &behavior, &expose, &policy, &retired); err != nil {
		return PublishedRoute{}, err
	}
	decodedOperations, err := decodeJSON[[]string](operations)
	if err != nil {
		return PublishedRoute{}, err
	}
	decodedTargets, err := decodeJSON[[]routing.PublishedTarget](targets)
	if err != nil {
		return PublishedRoute{}, err
	}
	decodedBehavior, err := routing.DecodeBehavior(behavior)
	if err != nil {
		return PublishedRoute{}, err
	}
	routingPolicy, err := DecodePolicy(policy)
	if err != nil {
		return PublishedRoute{}, err
	}
	return PublishedRoute{Retired: retired, OpenDraft: OpenDraft{
		ID: id, Slug: slug, ProjectID: project, Operations: decodedOperations, OverallTimeoutMS: timeout, MaxAttempts: attempts,
		Targets: decodedTargets, ContentPolicy: contentPolicy, Fidelity: fidelity, Behavior: decodedBehavior,
		ExposeUpstreamModels: expose, RoutingPolicy: routingPolicy,
	}}, nil
}

func decodeJSON[T any](raw []byte) (T, error) {
	var value T
	err := json.Unmarshal(raw, &value)
	return value, err
}

func encodePolicy(policy *routing.Policy) ([]byte, error) {
	if policy == nil {
		return nil, nil
	}
	return json.Marshal(policy)
}

func DecodePolicy(raw []byte) (*routing.Policy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	policy, err := decodeJSON[routing.Policy](raw)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}
