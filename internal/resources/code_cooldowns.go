package resources

import (
	"context"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *CodeStore) ObserveCooldownClass(ctx context.Context, accountID, model string, classification codeadapter.Classification, observedAt time.Time) error {
	classes := codeadapter.CooldownClasses()
	if !slices.Contains(classes[:], classification.Class) || classification.RetryAt.IsZero() || observedAt.IsZero() {
		return codemode.Refuse(400, "code_cooldown_invalid")
	}
	if classification.Scope != codeadapter.ScopeCredential && classification.Scope != codeadapter.ScopeModel {
		return codemode.Refuse(400, "code_cooldown_invalid")
	}
	if classification.Scope == codeadapter.ScopeModel && model == "" {
		return codemode.Refuse(400, "code_cooldown_invalid")
	}
	var storedModel *string
	if classification.Scope == codeadapter.ScopeModel {
		storedModel = &model
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO olp.code_account_cooldowns(account_id,model,class,retry_at,observed_at) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (account_id, (coalesce(model, ''))) DO UPDATE SET
			class=CASE WHEN excluded.retry_at>=code_account_cooldowns.retry_at THEN excluded.class ELSE code_account_cooldowns.class END,
			observed_at=CASE WHEN excluded.retry_at>=code_account_cooldowns.retry_at THEN excluded.observed_at ELSE code_account_cooldowns.observed_at END,
			retry_at=greatest(code_account_cooldowns.retry_at,excluded.retry_at)`,
		accountID, storedModel, string(classification.Class), classification.RetryAt, observedAt); err != nil {
		return err
	}
	if classification.Scope != codeadapter.ScopeCredential {
		return nil
	}
	status := "unavailable"
	if classification.Class == codeadapter.CooldownQuota {
		status = "quota_limited"
	}
	return s.ObserveHealth(ctx, accountID, codemode.HealthObservation{Status: status, ObservedAt: observedAt, RetryAt: &classification.RetryAt})
}
