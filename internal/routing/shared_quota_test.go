package routing

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func sharedQuotaProvider(projectID *string, quotas ...SharedTokenQuota) Provider {
	return Provider{ID: uuid.NewString(), ProjectID: projectID, SharedTokenQuotas: quotas}
}

func TestSharedTokenQuotaGroupIDIsStablePerProjectAndName(t *testing.T) {
	project := uuid.NewString()
	other := uuid.NewString()
	first := sharedQuotaProvider(&project, SharedTokenQuota{Model: "m", Group: "upstream-a", TokensPerMinute: 1000})
	second := sharedQuotaProvider(&project, SharedTokenQuota{Model: "other", Group: "upstream-a", TokensPerMinute: 1000})
	_, firstID, firstOK := first.SharedTokenQuotaFor("m")
	_, secondID, secondOK := second.SharedTokenQuotaFor("other")
	if !firstOK || !secondOK || firstID != secondID {
		t.Fatalf("same project and group resolved to %q and %q", firstID, secondID)
	}
	if SharedTokenQuotaGroupID(other, "upstream-a") == firstID {
		t.Fatal("another project resolved to the same group")
	}
}

func TestSnapshotRejectsInconsistentSharedTokenQuotas(t *testing.T) {
	project := uuid.NewString()
	snapshotWith := func(providers ...Provider) *Snapshot {
		snapshot := &Snapshot{Providers: map[string]Provider{}}
		for _, provider := range providers {
			snapshot.Providers[provider.ID] = provider
		}
		return snapshot
	}
	member := func(model, group string, ceiling int64) Provider {
		return sharedQuotaProvider(&project, SharedTokenQuota{Model: model, Group: group, TokensPerMinute: ceiling})
	}
	conflicting := snapshotWith(member("m", "g", 1000), member("m", "g", 500))
	if _, ok := errors.AsType[*ValidationError](conflicting.validateSharedTokenQuotas()); !ok {
		t.Fatal("members declaring different ceilings for one group were not refused as invalid configuration")
	}
	duplicate := snapshotWith(sharedQuotaProvider(&project,
		SharedTokenQuota{Model: "m", Group: "a", TokensPerMinute: 100},
		SharedTokenQuota{Model: "m", Group: "b", TokensPerMinute: 100}))
	if err := duplicate.validateSharedTokenQuotas(); err == nil {
		t.Fatal("one deployment model in two groups was accepted")
	}
	projectless := snapshotWith(sharedQuotaProvider(nil, SharedTokenQuota{Model: "m", Group: "g", TokensPerMinute: 100}))
	if err := projectless.validateSharedTokenQuotas(); err == nil {
		t.Fatal("a shared quota without a project was accepted")
	}
	consistent := snapshotWith(member("m", "g", 1000), member("x", "g", 1000))
	if err := consistent.validateSharedTokenQuotas(); err != nil {
		t.Fatalf("consistent members were rejected: %v", err)
	}
}
