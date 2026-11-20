package codeaccounts

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type Settings struct {
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Models            []string `json:"models"`
	ExtraUsageEnabled *bool    `json:"extra_usage_enabled"`
	CycleAnchor       *string  `json:"cycle_anchor,omitempty"`
	MaxConcurrency    *int32   `json:"max_concurrency,omitempty"`
}

type Input struct {
	ProjectID    string `json:"project_id"`
	ProviderID   string `json:"provider_id"`
	CredentialID string `json:"credential_id"`
	Settings
}

type Account struct {
	ID       string
	Project  string
	Provider string
	Settings
}

type Granted struct {
	ProviderID string
	Principal  string
}

func (s Settings) Validate() error {
	if err := httpkit.ValidText("name", s.Name, 100); err != nil {
		return err
	}
	if err := codemode.ValidateModels(s.Models); err != nil {
		return httpkit.Invalid("models", err.Error())
	}
	if s.MaxConcurrency != nil && *s.MaxConcurrency < 1 {
		return httpkit.Invalid("max_concurrency", "Set a positive generation limit or leave it unset.")
	}
	if s.CycleAnchor == nil {
		return nil
	}
	if _, ok := codemode.ParseCycleAnchor(*s.CycleAnchor); !ok {
		return httpkit.Invalid("cycle_anchor", "Use a calendar date as the subscription cycle anchor.")
	}
	return nil
}

func (in Input) validated() (Input, error) {
	ids, err := httpkit.ParseUUIDs([]string{in.ProjectID, in.ProviderID, in.CredentialID})
	if err != nil {
		return Input{}, err
	}
	return Input{ProjectID: ids[0], ProviderID: ids[1], CredentialID: ids[2], Settings: in.Settings}, in.Settings.Validate()
}

func ReadAccounts(ctx context.Context, q database.Queryer) ([]Account, error) {
	rows, err := q.Query(ctx, `SELECT a.id::text,p.name,v.name,a.name,a.enabled,a.models,a.extra_usage_enabled,a.cycle_anchor::text,a.max_concurrency FROM olp.code_accounts a JOIN olp.projects p ON p.id=a.project_id JOIN olp.providers v ON v.id=a.provider_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Account])
}

func AccountIDs(ctx context.Context, q database.Queryer, projectID, providerID, principal string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM olp.code_accounts WHERE project_id=$1 AND provider_id=$2 AND principal=$3`, projectID, providerID, principal)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func ReadInput(ctx context.Context, q database.Queryer, id string) (Input, error) {
	var in Input
	err := q.QueryRow(ctx, `SELECT project_id::text,provider_id::text,credential_id::text,name,enabled,models,extra_usage_enabled,cycle_anchor::text,max_concurrency FROM olp.code_accounts WHERE id=$1`, id).
		Scan(&in.ProjectID, &in.ProviderID, &in.CredentialID, &in.Name, &in.Enabled, &in.Models, &in.ExtraUsageEnabled, &in.CycleAnchor, &in.MaxConcurrency)
	return in, err
}

func ReadGranted(ctx context.Context, q database.Queryer, providerName, projectID, credentialID string) (Granted, bool, error) {
	var granted Granted
	err := q.QueryRow(ctx, `SELECT p.id::text,c.principal FROM olp.providers p JOIN olp.provider_credentials c ON c.provider_id=p.id JOIN olp.provider_grants g ON g.credential_id=c.id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND c.id=$3 AND c.principal<>'' AND c.revoked_at IS NULL AND `+grants.LiveCondition+``, providerName, projectID, credentialID).Scan(&granted.ProviderID, &granted.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return Granted{}, false, nil
	}
	return granted, err == nil, err
}

const remainingPins = `SELECT count(*) FROM olp.code_pins pin
	JOIN olp.code_bindings b ON b.id=pin.root_id
	JOIN olp.api_keys k ON k.id=b.api_key_id
	JOIN olp.code_routes r ON r.id=b.route_id
	JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
	JOIN olp.code_pool_accounts pa ON pa.pool_id=(v.document->>'pool_id')::uuid AND pa.account_id=pin.account_id
	JOIN olp.code_pool_keys pk ON pk.pool_id=pa.pool_id AND pk.api_key_id=k.id
	WHERE pin.account_id=x.id AND pin.principal=x.principal AND b.retired_at IS NULL
	AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>now())
	AND v.document->>'enabled'='true' AND v.connections ? x.provider_id::text
	AND v.document->'models' ? coalesce(pin.target_model,pin.model)
	AND x.models ? coalesce(pin.target_model,pin.model)`

var accountJSON = `SELECT jsonb_build_object('id',x.id,'project_id',x.project_id,'provider_id',x.provider_id,'credential_id',x.credential_id,'principal',x.principal,'name',x.name,'enabled',x.enabled,'models',x.models,'extra_usage_enabled',x.extra_usage_enabled,'etag',x.etag,'health',x.health,'health_observed_at',x.health_observed_at,'unavailable_until',x.unavailable_until,'cycle_anchor',x.cycle_anchor,
	'draining',x.draining,
	'remaining_pins',CASE WHEN x.enabled AND p.state<>'disabled' AND c.revoked_at IS NULL AND ` + grants.LiveCondition + ` THEN (` + remainingPins + `) ELSE 0 END,
	'last_activity_at',(SELECT d.created_at FROM olp.code_decisions d WHERE d.chosen_account_id=x.id ORDER BY d.created_at DESC LIMIT 1),
	'max_concurrency',x.max_concurrency,
	'cooldowns',coalesce((SELECT jsonb_agg(jsonb_build_object('class',cd.class,'model',cd.model,'retry_at',cd.retry_at) ORDER BY cd.retry_at,cd.class,cd.model) FROM olp.code_account_cooldowns cd WHERE cd.account_id=x.id AND cd.retry_at>now()),'[]'::jsonb),
	'adapter',` + codeadapter.SQL("p.configuration") + `,
	'allowance_signal',` + codeadapter.SQLAllowanceSignal("p.configuration") + `,
	'allowance_signal_reason',` + codeadapter.SQLAllowanceSignalReason("p.configuration") + `,
	'research_services',CASE WHEN c.profile_id='` + codeplans.ZAIProfile + `' AND p.configuration->>'profile_id'=c.profile_id THEN '["search","reader","zread"]'::jsonb ELSE '[]'::jsonb END,
	'research_eligible',coalesce(x.enabled AND p.state<>'disabled' AND p.project_id=x.project_id
		AND c.provider_id=x.provider_id AND c.principal=x.principal AND c.profile_id='` + codeplans.ZAIProfile + `'
		AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND g.expires_at IS NULL AND g.refresh_token_id IS NULL
		AND p.configuration->>'kind'='plugin' AND p.configuration->>'auth_mode'='grant' AND p.configuration->>'profile_id'=c.profile_id,false),
	'allowance',x.allowance,'allowance_read_requested_at',x.allowance_read_requested_at,'allowance_read_at',x.allowance_read_at,'eligible',coalesce(x.enabled AND NOT x.draining AND p.state<>'disabled' AND olp.code_account_available(x) AND c.revoked_at IS NULL AND ` + grants.LiveCondition + `
		AND EXISTS(SELECT 1 FROM jsonb_array_elements_text(x.models) m(model) WHERE NOT EXISTS(SELECT 1 FROM olp.code_account_cooldowns cd WHERE cd.account_id=x.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=m.model))),false),
	'grant_state',CASE WHEN c.revoked_at IS NOT NULL THEN 'revoked' WHEN g.lapsed_at IS NOT NULL THEN 'lapsed' WHEN g.expires_at<=now() THEN 'expired' ELSE 'current' END)
	FROM olp.code_accounts x JOIN olp.providers p ON p.id=x.provider_id JOIN olp.provider_credentials c ON c.id=x.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id`

func Save(ctx context.Context, tx pgx.Tx, p authz.Principal, id, etag string, input Input, existing bool) (json.RawMessage, error) {
	in, err := input.validated()
	if err != nil {
		return nil, err
	}
	return save(ctx, tx, p, id, etag, in, existing)
}

func save(ctx context.Context, tx pgx.Tx, p authz.Principal, id, etag string, in Input, existing bool) (json.RawMessage, error) {
	if err := p.Project(&in.ProjectID, authz.Change); err != nil {
		return nil, err
	}
	var principal string
	var adapter *string
	var profileID *string
	err := tx.QueryRow(ctx, `SELECT c.principal,`+codeadapter.SQL("p.configuration")+`,p.configuration->>'profile_id' FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id JOIN olp.provider_grants g ON g.credential_id=c.id
			WHERE p.id=$1 AND p.project_id=$2 AND c.id=$3 AND (NOT $4 OR (c.revoked_at IS NULL AND `+grants.LiveCondition+`))`, in.ProviderID, in.ProjectID, in.CredentialID, in.Enabled).Scan(&principal, &adapter, &profileID)
	if err != nil {
		return nil, err
	}
	if principal == "" {
		return nil, httpkit.Invalid("credential_id", "Enrollment must establish an upstream principal.")
	}
	if in.ExtraUsageEnabled != nil && (adapter == nil || *adapter != string(codemode.AdapterKimiCode)) {
		return nil, httpkit.Invalid("extra_usage_enabled", "Extra Usage applies only to Kimi Code accounts.")
	}
	if in.CycleAnchor != nil && (profileID == nil || *profileID != codeplans.QwenTokenPlanProfile && *profileID != codeplans.QwenTokenPlanCNProfile) {
		return nil, httpkit.Invalid("cycle_anchor", "A cycle anchor applies only to Qwen Token Plan accounts.")
	}
	if existing {
		var oldPrincipal, oldProvider string
		if err = tx.QueryRow(ctx, `SELECT principal,provider_id::text FROM olp.code_accounts WHERE id=$1`, id).Scan(&oldPrincipal, &oldProvider); err != nil {
			return nil, err
		}
		if oldPrincipal != principal || oldProvider != in.ProviderID {
			return nil, httpkit.Invalid("credential_id", "Account identity cannot change during credential rotation.")
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,enabled,models,extra_usage_enabled,etag,created_by,cycle_anchor,max_concurrency) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT(id) DO UPDATE SET credential_id=excluded.credential_id,name=excluded.name,enabled=excluded.enabled,models=excluded.models,extra_usage_enabled=excluded.extra_usage_enabled,etag=excluded.etag,cycle_anchor=excluded.cycle_anchor,max_concurrency=excluded.max_concurrency,
			health=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN 'unknown' ELSE code_accounts.health END,
			health_observed_at=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN NULL ELSE code_accounts.health_observed_at END,
			unavailable_until=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN NULL ELSE code_accounts.unavailable_until END`, id, in.ProjectID, in.ProviderID, in.CredentialID, principal, in.Name, in.Enabled, in.Models, in.ExtraUsageEnabled, etag, p.UserID(), in.CycleAnchor, in.MaxConcurrency)
	if err != nil {
		return nil, err
	}
	var body json.RawMessage
	err = tx.QueryRow(ctx, accountJSON+` WHERE x.id=$1`, id).Scan(&body)
	return body, err
}
