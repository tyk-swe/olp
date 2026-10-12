//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestPendingUsageUnknownVersionRetainsOwnerWithoutStarvingExpiry(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	local, original, owner := pendingGeminiGeneration(t, h, slug, key)
	id := strings.TrimPrefix(local, "interaction_")
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources
 SET metadata=jsonb_set(metadata,'{pending_usage,version}','2'::jsonb) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	input, output := int64(3), int64(2)
	for _, status := range []string{"in_progress", "completed"} {
		evidence := usage.AttemptUsage{Observed: true, Complete: status == "completed", BillingUncertain: status != "completed", InputTokens: &input, OutputTokens: &output}
		if _, err := h.Gateway.Resources.ObserveInteraction(t.Context(), owner, local, status, evidence, time.Now()); !errors.Is(err, usage.ErrUnsupportedVersion) {
			t.Fatalf("unknown accounting version was reinterpreted during %s: %v", status, err)
		}
	}
	resource, err := h.Gateway.Resources.Get(t.Context(), resources.KindInteraction, owner, local)
	if err != nil {
		t.Fatal(err)
	}
	// Make the unsupported owner the oldest expired row. More than one batch
	// of unrelated, valid encrypted mappings must still reclaim their secrets.
	const validCount = 105
	valid := make([]string, 0, validCount)
	for i := range validCount {
		copy := *resource
		copy.Metadata = json.RawMessage(`{"upstream_model":"review-model"}`)
		expiry := time.Now().Add(time.Minute)
		copy.ExpiresAt = &expiry
		stored, err := h.Gateway.Resources.PutInteractionContract(t.Context(), &copy, fmt.Sprintf("review-native-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		valid = append(valid, stored.UUID.String())
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET expires_at=now()-interval '2 minutes' WHERE id=ANY($1::uuid[])`, valid); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET expires_at=now()-interval '3 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	removed, err := h.Gateway.Resources.Expire(t.Context(), time.Now())
	var failed *resources.ExpiryFailures
	if removed != validCount || !errors.Is(err, usage.ErrUnsupportedVersion) || !errors.As(err, &failed) || failed.Count != 1 {
		t.Fatalf("unrelated expiry did not progress around preserved poison: removed=%d failure=%v", removed, err)
	}
	var mappings, ownedSecrets, poisonSecrets, facts, version int
	for _, query := range []struct {
		sql  string
		args []any
		into *int
	}{
		{`SELECT count(*) FROM olp.provider_resources WHERE id=ANY($1::uuid[])`, []any{valid}, &mappings},
		{`SELECT count(*) FROM olp.secrets WHERE id=ANY($1::uuid[]) AND purpose=$2`, []any{valid, secrets.ProviderContinuation}, &ownedSecrets},
		{`SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose=$2`, []any{id, secrets.ProviderContinuation}, &poisonSecrets},
		{`SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, []any{original}, &facts},
		{`SELECT (metadata->'pending_usage'->>'version')::integer FROM olp.provider_resources WHERE id=$1`, []any{id}, &version},
	} {
		if err := h.Pool.QueryRow(t.Context(), query.sql, query.args...).Scan(query.into); err != nil {
			t.Fatal(err)
		}
	}
	if mappings != 0 || ownedSecrets != 0 || poisonSecrets != 1 || facts != 0 || version != 2 {
		t.Fatalf("expiry erased evidence or retained unrelated ownership: mappings=%d secrets=%d poisonSecret=%d facts=%d version=%d", mappings, ownedSecrets, poisonSecrets, facts, version)
	}
	if err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); !errors.Is(err, resources.ErrPendingUsage) {
		t.Fatalf("unknown accepted generation lost its cost quarantine: %v", err)
	}
	if removed, err = h.Gateway.Resources.Expire(t.Context(), time.Now()); removed != 0 || !errors.Is(err, usage.ErrUnsupportedVersion) {
		t.Fatalf("next pass must retry preserved unknown owner: removed=%d error=%v", removed, err)
	}
}
