package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	configurationLimit = 4096
	signingSecretLimit = 1024
	secretObjectLimit  = 16384
	nameLimit          = 100
)

type resource struct {
	table, parameter, kind, path string
	alias, fields, from          string
	remove                       func(context.Context, pgx.Tx, string) error
}

var destinations = resource{
	table: "notification_destinations", parameter: "notification_destination_id", kind: "notification_destination",
	path: "/api/v1/notifications/destinations/", alias: "d", fields: destinationFields, from: destinationFrom,
	remove: removeDestination,
}

var rules = resource{
	table: "notification_rules", parameter: "notification_rule_id", kind: "notification_rule",
	path: "/api/v1/notifications/rules/", alias: "r", fields: ruleFields, from: ruleFrom,
	remove: removeRule,
}

func (res resource) read(ctx context.Context, tx pgx.Tx, id string) (json.RawMessage, error) {
	var data []byte
	err := tx.QueryRow(ctx, "SELECT jsonb_build_object("+res.fields+")"+res.from+" WHERE "+res.alias+".id=$1", id).Scan(&data)
	return data, err
}

func removeRule(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.notification_rules WHERE id=$1", id)
	return err
}

func removeDestination(ctx context.Context, tx pgx.Tx, id string) error {
	var secretID *string
	if err := tx.QueryRow(ctx, deleteDestinationSQL, id).Scan(&secretID); err != nil || secretID == nil {
		return err
	}
	return deleteSecret(ctx, tx, *secretID)
}

func deleteSecret(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, secrets.NotificationSecret)
	return err
}

func referenceProject(ctx context.Context, tx pgx.Tx, query, id string) (*string, bool, error) {
	var project *string
	err := tx.QueryRow(ctx, query, id).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return project, err == nil, err
}

func sameProject(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Server) storeNotificationSecret(r *http.Request, tx pgx.Tx, id string, secret []byte) error {
	var secretID *string
	if secret != nil {
		stored := access.NewID()
		if err := s.access.Keys.Store(r.Context(), tx, s.access.Installation, stored, secrets.NotificationSecret, secret, nil); err != nil {
			return err
		}
		secretID = &stored
	}
	var previous *string
	if err := tx.QueryRow(r.Context(), readDestinationSecretSQL, id).Scan(&previous); err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(),
		"UPDATE olp.notification_destinations SET secret_id=$2 WHERE id=$1", id, secretID); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	return deleteSecret(r.Context(), tx, *previous)
}

const destinationFields = `'id',d.id,'name',d.name,'url',d.url,'type',d.type,'configuration',d.configuration,'secret_configured',d.secret_id IS NOT NULL,'project_id',d.project_id,'project_name',p.name,'enabled',d.enabled,'etag',d.etag,'created_by',d.created_by,'created_by_email',u.email,'created_at',d.created_at,'updated_at',d.updated_at`
const destinationFrom = ` FROM olp.notification_destinations d
	JOIN olp.users u ON u.id=d.created_by LEFT JOIN olp.projects p ON p.id=d.project_id`
const ruleFields = `'id',r.id,'name',r.name,'project_id',r.project_id,'project_name',p.name,'event',r.event,'configuration',r.configuration,'subject_kind',r.subject_kind,'subject_id',r.subject_id,'subject_name',COALESCE(k.name,g.name),'window_kind',r.window_kind,'threshold_percent',r.threshold_percent,'destination_id',r.destination_id,'destination_name',d.name,'enabled',r.enabled,'etag',r.etag,'created_by',r.created_by,'created_by_email',u.email,'created_at',r.created_at,'updated_at',r.updated_at`

const ruleFrom = ` FROM olp.notification_rules r
	JOIN olp.users u ON u.id=r.created_by
	LEFT JOIN olp.projects p ON p.id=r.project_id
	LEFT JOIN olp.api_keys k ON r.subject_kind='api_key' AND k.id=r.subject_id
	LEFT JOIN olp.budget_groups g ON r.subject_kind='budget_group' AND g.id=r.subject_id
	JOIN olp.notification_destinations d ON d.id=r.destination_id`

const createChannelRuleSQL = `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,enabled,etag,created_by,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`

const updateChannelRuleSQL = `UPDATE olp.notification_rules SET name=$2,subject_kind=$3,subject_id=$4,window_kind=$5,threshold_percent=$6,destination_id=$7,enabled=$8,etag=$9,configuration=$10,updated_at=now() WHERE id=$1`

const lockGenericRuleCreationSQL = `SELECT pg_advisory_xact_lock(hashtextextended('olp.notification_rule_creation',0))`

const createChannelDestinationSQL = `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by,enabled,type,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
const updateChannelDestinationSQL = `UPDATE olp.notification_destinations SET name=$2,url=$3,enabled=$4,etag=$5,type=$6,configuration=$7,updated_at=now() WHERE id=$1`
const readDestinationSecretSQL = `SELECT secret_id::text FROM olp.notification_destinations WHERE id=$1`

const deleteDestinationSQL = `DELETE FROM olp.notification_destinations WHERE id=$1 RETURNING secret_id::text`

const evaluatedRuleCountSQL = `SELECT count(*) FROM olp.notification_rules WHERE event=ANY($1)`

const (
	apiKeyProjectSQL      = `SELECT project_id::text FROM olp.api_keys WHERE id=$1`
	budgetGroupProjectSQL = `SELECT project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL`
	destinationProjectSQL = `SELECT project_id::text FROM olp.notification_destinations WHERE id=$1`
)
