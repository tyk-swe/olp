package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
)

type View struct {
	ID                       string                 `json:"id"`
	ProjectID                string                 `json:"project_id"`
	ProviderID               string                 `json:"provider_id"`
	CredentialID             string                 `json:"credential_id"`
	Principal                string                 `json:"principal"`
	Name                     string                 `json:"name"`
	Enabled                  bool                   `json:"enabled"`
	Models                   []string               `json:"models"`
	ExtraUsageEnabled        *bool                  `json:"extra_usage_enabled"`
	ETag                     string                 `json:"etag"`
	Health                   code.Health            `json:"health"`
	HealthObservedAt         *time.Time             `json:"health_observed_at"`
	UnavailableUntil         *time.Time             `json:"unavailable_until"`
	CycleAnchor              *string                `json:"cycle_anchor"`
	NextMonthlyReset         *string                `json:"next_monthly_reset,omitempty"`
	Cooldowns                []CooldownView         `json:"cooldowns"`
	Adapter                  *code.Adapter          `json:"adapter"`
	AllowanceSignal          vendor.AllowanceSignal `json:"allowance_signal"`
	AllowanceSignalReason    *string                `json:"allowance_signal_reason"`
	ResearchServices         []string               `json:"research_services"`
	ResearchEligible         bool                   `json:"research_eligible"`
	Allowance                *code.Allowance        `json:"allowance"`
	AllowanceReadRequestedAt *time.Time             `json:"allowance_read_requested_at"`
	AllowanceReadAt          *time.Time             `json:"allowance_read_at"`
	Eligible                 bool                   `json:"eligible"`
	GrantState               code.GrantState        `json:"grant_state"`
}

type CooldownView struct {
	Class   code.CooldownClass `json:"class"`
	Model   *string            `json:"model"`
	RetryAt time.Time          `json:"retry_at"`
}

const selectRows = `SELECT
	x.id::text, x.project_id::text, x.provider_id::text, x.credential_id::text, x.principal,
	x.name, x.enabled, x.models, x.extra_usage_enabled, x.etag::text, x.health,
	x.health_observed_at, x.unavailable_until, x.cycle_anchor::text,
	coalesce((SELECT jsonb_agg(jsonb_build_object('class',cd.class,'model',cd.model,'retry_at',cd.retry_at) ORDER BY cd.retry_at,cd.class,cd.model) FROM olp.code_account_cooldowns cd WHERE cd.account_id=x.id AND cd.retry_at>now()),'[]'::jsonb),
	p.configuration, c.profile_id,
	coalesce(x.enabled AND p.state<>'disabled' AND p.project_id=x.project_id
		AND c.provider_id=x.provider_id AND c.principal=x.principal
		AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND g.expires_at IS NULL AND g.refresh_token_id IS NULL, false),
	x.allowance, x.allowance_read_requested_at, x.allowance_read_at,
	coalesce(x.enabled AND p.state<>'disabled' AND olp.code_account_available(x) AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
		AND EXISTS(SELECT 1 FROM jsonb_array_elements_text(x.models) m(model) WHERE NOT EXISTS(SELECT 1 FROM olp.code_account_cooldowns cd WHERE cd.account_id=x.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=m.model))), false),
	CASE WHEN c.revoked_at IS NOT NULL THEN 'revoked' WHEN g.lapsed_at IS NOT NULL THEN 'lapsed' WHEN g.expires_at<=now() THEN 'expired' ELSE 'current' END
	FROM olp.code_accounts x
	JOIN olp.providers p ON p.id=x.provider_id
	JOIN olp.provider_credentials c ON c.id=x.credential_id
	JOIN olp.provider_grants g ON g.credential_id=c.id`

func scanView(row pgx.Row, now time.Time) (View, error) {
	var v View
	var cooldownsJSON, configJSON, allowanceJSON []byte
	var credentialProfile *string
	var researchBaseEligible bool
	err := row.Scan(
		&v.ID, &v.ProjectID, &v.ProviderID, &v.CredentialID, &v.Principal,
		&v.Name, &v.Enabled, &v.Models, &v.ExtraUsageEnabled, &v.ETag, &v.Health,
		&v.HealthObservedAt, &v.UnavailableUntil, &v.CycleAnchor,
		&cooldownsJSON, &configJSON, &credentialProfile, &researchBaseEligible,
		&allowanceJSON, &v.AllowanceReadRequestedAt, &v.AllowanceReadAt,
		&v.Eligible, &v.GrantState,
	)
	if err != nil {
		return View{}, err
	}
	v.Cooldowns = []CooldownView{}
	if len(cooldownsJSON) > 0 {
		if err := json.Unmarshal(cooldownsJSON, &v.Cooldowns); err != nil {
			return View{}, err
		}
	}
	if len(allowanceJSON) > 0 {
		var allowance code.Allowance
		if err := json.Unmarshal(allowanceJSON, &allowance); err != nil {
			return View{}, err
		}
		v.Allowance = &allowance
	}
	var cfg vendor.ConnectionIdentity
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return View{}, err
		}
	}
	v.AllowanceSignal = vendor.AllowanceSignalNone
	v.ResearchServices = []string{}
	if adapter, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID); ok {
		a := adapter.Adapter
		v.Adapter = &a
		sig := adapter.AllowanceSignal(cfg.ProfileID)
		v.AllowanceSignal = sig.Signal
		if sig.Reason != "" {
			reason := sig.Reason
			v.AllowanceSignalReason = &reason
		}
		if credentialProfile != nil && *credentialProfile == cfg.ProfileID && adapter.SupportsResearch(*credentialProfile) {
			services := code.ResearchServices()
			v.ResearchServices = services[:]
			v.ResearchEligible = researchBaseEligible
		}
	}
	v.NextMonthlyReset = code.NextMonthlyResetDate(v.CycleAnchor, now)
	return v, nil
}

func Load(ctx context.Context, q access.Queryer, id string) (View, error) {
	return scanView(q.QueryRow(ctx, selectRows+` WHERE x.id=$1`, id), time.Now().UTC())
}

func Save(ctx context.Context, tx pgx.Tx, p access.Principal, id, etag string, d Draft, existing bool) (View, error) {
	project := d.projectID
	if err := p.Project(&project, access.Change); err != nil {
		return View{}, err
	}
	var principal string
	var configJSON []byte
	err := tx.QueryRow(ctx, `SELECT c.principal, p.configuration
		FROM olp.provider_credentials c
		JOIN olp.providers p ON p.id=c.provider_id
		JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE p.id=$1 AND p.project_id=$2 AND c.id=$3
		AND (NOT $4 OR (c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())))`,
		d.providerID, d.projectID, d.credentialID, d.enabled).Scan(&principal, &configJSON)
	if err != nil {
		return View{}, err
	}
	if principal == "" {
		return View{}, access.Invalid("credential_id", "Enrollment must establish an upstream principal.")
	}
	var cfg vendor.ConnectionIdentity
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return View{}, err
		}
	}
	adapter, hasAdapter := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if d.extraUsageEnabled != nil && (!hasAdapter || !adapter.ExtraUsageAllowed) {
		return View{}, access.Invalid("extra_usage_enabled", "Extra Usage applies only to Kimi Code accounts.")
	}
	if d.cycleAnchor != nil && (!hasAdapter || !adapter.SupportsCycleAnchor(cfg.ProfileID)) {
		return View{}, access.Invalid("cycle_anchor", "A cycle anchor applies only to Qwen Token Plan accounts.")
	}
	if existing {
		var oldPrincipal, oldProvider string
		if err = tx.QueryRow(ctx, `SELECT principal,provider_id::text FROM olp.code_accounts WHERE id=$1`, id).Scan(&oldPrincipal, &oldProvider); err != nil {
			return View{}, err
		}
		if oldPrincipal != principal || oldProvider != d.providerID {
			return View{}, access.Invalid("credential_id", "Account identity cannot change during credential rotation.")
		}
	}
	models, err := json.Marshal(d.models)
	if err != nil {
		return View{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,enabled,models,extra_usage_enabled,etag,created_by,cycle_anchor)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT(id) DO UPDATE SET credential_id=excluded.credential_id,name=excluded.name,enabled=excluded.enabled,models=excluded.models,extra_usage_enabled=excluded.extra_usage_enabled,etag=excluded.etag,cycle_anchor=excluded.cycle_anchor,
		health=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN 'unknown' ELSE code_accounts.health END,
		health_observed_at=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN NULL ELSE code_accounts.health_observed_at END,
		unavailable_until=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN NULL ELSE code_accounts.unavailable_until END`,
		id, d.projectID, d.providerID, d.credentialID, principal, d.name, d.enabled, models, d.extraUsageEnabled, etag, p.UserID(), d.cycleAnchor)
	if err != nil {
		return View{}, err
	}
	return Load(ctx, tx, id)
}

type Stored struct {
	ID                string
	Project           string
	Provider          string
	Name              string
	Enabled           bool
	Models            []string
	ExtraUsageEnabled *bool
	CycleAnchor       *string
}

type GrantedCredential struct {
	ProviderID string
	Principal  string
}

func ReadStored(ctx context.Context, q access.Queryer) ([]Stored, error) {
	rows, err := q.Query(ctx, `SELECT a.id::text,p.name,v.name,a.name,a.enabled,a.models,a.extra_usage_enabled,a.cycle_anchor::text FROM olp.code_accounts a JOIN olp.projects p ON p.id=a.project_id JOIN olp.providers v ON v.id=a.provider_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Stored])
}

func IDs(ctx context.Context, q access.Queryer, projectID, providerID, principal string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM olp.code_accounts WHERE project_id=$1 AND provider_id=$2 AND principal=$3`, projectID, providerID, principal)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func ReadInput(ctx context.Context, q access.Queryer, id string) (Input, error) {
	var input Input
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'provider_id',provider_id,'credential_id',credential_id,'name',name,'enabled',enabled,'models',models,'extra_usage_enabled',extra_usage_enabled,'cycle_anchor',cycle_anchor) FROM olp.code_accounts WHERE id=$1`, id).Scan(&input)
	return input, err
}

func ReadGrantedCredential(ctx context.Context, q access.Queryer, providerName, projectID, credentialID string) (*GrantedCredential, error) {
	var granted GrantedCredential
	err := q.QueryRow(ctx, `SELECT p.id::text,c.principal FROM olp.providers p JOIN olp.provider_credentials c ON c.provider_id=p.id JOIN olp.provider_grants g ON g.credential_id=c.id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND c.id=$3 AND c.principal<>'' AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())`, providerName, projectID, credentialID).Scan(&granted.ProviderID, &granted.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &granted, nil
}
