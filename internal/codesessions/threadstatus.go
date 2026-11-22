package codesessions

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/grants"
)

func threadNotFound() error {
	return codemode.Refuse(404, "code_thread_not_found")
}

func (s *Store) ThreadStatus(ctx context.Context, route codemode.Route, key string, identity codemode.Identity, now time.Time) (codemode.ThreadStatus, error) {
	var none codemode.ThreadStatus
	if err := identity.Validate(); err != nil {
		return none, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return none, err
	}
	defer tx.Rollback(ctx)
	in := Admission{Route: route, APIKeyID: key}
	var authority keyauthority.Authority
	current, err := codeAuthorityDocument(ctx, tx, in, &authority, codeThreadStatus)
	if err != nil {
		if _, refusal := errors.AsType[*codemode.Refusal](err); refusal {
			return none, threadNotFound()
		}
		return none, err
	}
	binding, err := codeConversation(ctx, tx, in, identity.Conversation)
	if errors.Is(err, pgx.ErrNoRows) {
		return paidThreadStatus(ctx, tx, in, identity, now.UTC())
	}
	if err != nil {
		return none, err
	}
	if err := threadLineage(ctx, tx, `olp.code_bindings`, binding.ParentID, identity); err != nil {
		return none, err
	}
	retired, err := threadRetired(ctx, tx, binding)
	if err != nil {
		return none, err
	}
	pins, err := codePins(ctx, tx, binding.RootID)
	if err != nil {
		return none, err
	}
	if slices.ContainsFunc(pins, func(pin codemode.Pin) bool {
		_, err := current.ResolvePinnedModel(codemode.Operation{Model: pin.Model}, []codemode.Pin{pin})
		return err != nil || !slices.Contains(route.Models, pin.TargetModel)
	}) {
		return codemode.ThreadStatus{Status: codemode.ThreadUnavailable, CheckedAt: now.UTC()}, nil
	}
	tree, principals := codeTree(binding, pins)
	evidence := make([]codemode.ThreadAccountEvidence, 0, len(tree))
	for _, accountID := range tree {
		models := slices.Collect(func(yield func(string) bool) {
			for _, pin := range pins {
				if pin.AccountID == accountID && !yield(pin.TargetModel) {
					return
				}
			}
		})
		account, err := threadAccountEvidence(ctx, tx, route, accountID, principals[accountID], models)
		if err != nil {
			return none, err
		}
		evidence = append(evidence, account)
	}
	return codemode.AssessThreadStatus(now.UTC(), retired, evidence), nil
}

func threadLineage(ctx context.Context, tx pgx.Tx, table string, parentID *string, identity codemode.Identity) error {
	if identity.Parent == "" {
		return nil
	}
	if parentID == nil {
		return threadNotFound()
	}
	var parent string
	err := tx.QueryRow(ctx, `SELECT conversation FROM `+table+` WHERE id=$1`, parentID).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && parent != identity.Parent {
		return threadNotFound()
	}
	return err
}

func threadRetired(ctx context.Context, tx pgx.Tx, binding codemode.Binding) (bool, error) {
	if binding.RetiredAt != nil {
		return true, nil
	}
	var retired bool
	err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM olp.code_bindings WHERE id=$1`, binding.RootID).Scan(&retired)
	return retired, err
}

func threadAccountEvidence(ctx context.Context, tx pgx.Tx, route codemode.Route, accountID, principal string, models []string) (codemode.ThreadAccountEvidence, error) {
	var account codemode.Account
	var eligible bool
	var cooldownRetryAt, cooldownObservedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT a.principal,a.models,a.health,a.health_observed_at,a.unavailable_until,a.allowance,
		(a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id AND a.project_id=$2
			AND EXISTS(SELECT 1 FROM olp.code_pool_accounts pa WHERE pa.pool_id=$3 AND pa.account_id=a.id)
			AND EXISTS(SELECT 1 FROM olp.code_route_revisions v WHERE v.id=$5 AND v.connections->(a.provider_id::text) IS NOT NULL)
			AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND `+grants.LiveCondition+`),
		(SELECT max(cd.retry_at) FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND (cd.model IS NULL OR cd.model=ANY($4::text[]))),
		(SELECT max(cd.observed_at) FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND (cd.model IS NULL OR cd.model=ANY($4::text[])))
		FROM olp.code_accounts a
		JOIN olp.providers p ON p.id=a.provider_id
		JOIN olp.provider_credentials c ON c.id=a.credential_id
		JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE a.id=$1`, accountID, route.ProjectID, route.PoolID, models, route.RevisionID).Scan(
		&account.Principal, &account.Models, &account.Health, &account.HealthObservedAt, &account.UnavailableUntil, &account.Allowance,
		&eligible, &cooldownRetryAt, &cooldownObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return codemode.ThreadAccountEvidence{}, nil
	}
	if err != nil {
		return codemode.ThreadAccountEvidence{}, err
	}
	return codemode.ThreadAccountEvidence{
		Eligible: eligible && account.Principal == principal &&
			!slices.ContainsFunc(models, func(model string) bool { return !slices.Contains(account.Models, model) }),
		Health:             account.Health,
		HealthObservedAt:   account.HealthObservedAt,
		UnavailableUntil:   account.UnavailableUntil,
		Allowance:          account.Allowance,
		CooldownRetryAt:    cooldownRetryAt,
		CooldownObservedAt: cooldownObservedAt,
	}, nil
}

func paidThreadStatus(ctx context.Context, tx pgx.Tx, in Admission, identity codemode.Identity, now time.Time) (codemode.ThreadStatus, error) {
	var none codemode.ThreadStatus
	var parentID *string
	var retired bool
	err := tx.QueryRow(ctx, `SELECT b.parent_id::text, root.retired_at IS NOT NULL OR subscription.retired_at IS NOT NULL
		FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id
		LEFT JOIN olp.code_bindings subscription ON subscription.id=root.subscription_root_id
		WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3 ORDER BY b.id LIMIT 1`,
		in.Route.ID, in.APIKeyID, identity.Conversation).Scan(&parentID, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return none, threadNotFound()
	}
	if err != nil {
		return none, err
	}
	if err := threadLineage(ctx, tx, `olp.code_paid_bindings`, parentID, identity); err != nil {
		return none, err
	}
	if retired {
		return codemode.ThreadStatus{Status: codemode.ThreadUnavailable, CheckedAt: now}, nil
	}
	return codemode.ThreadStatus{Status: codemode.ThreadUnknown, CheckedAt: now}, nil
}
