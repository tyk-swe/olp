package identity

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
)

func TestValidManagementTokenDoesNotMutateProjectIDs(t *testing.T) {
	t.Parallel()
	now := time.Unix(1750000000, 0).UTC()
	expires := now.Add(24 * time.Hour)
	rawIDs := []string{
		"01900000-0000-7000-8000-00000000000A",
		"01900000-0000-7000-8000-00000000000B",
	}
	snapshot := slices.Clone(rawIDs)
	scope := authz.TokenScopes()[0].String()

	normalized, err := validManagementTokenAt(managementTokenInput{
		Name:       "ci-token",
		Scopes:     []string{scope},
		ExpiresAt:  &expires,
		ProjectIDs: &rawIDs,
	}, now)
	if err != nil {
		t.Fatalf("validManagementTokenAt returned unexpected error: %v", err)
	}
	want := []string{
		"01900000-0000-7000-8000-00000000000a",
		"01900000-0000-7000-8000-00000000000b",
	}
	if !slices.Equal(normalized, want) {
		t.Fatalf("normalized = %v, want %v", normalized, want)
	}
	if !slices.Equal(rawIDs, snapshot) {
		t.Fatalf("input ProjectIDs mutated to %v, want unchanged %v", rawIDs, snapshot)
	}
}

func TestValidManagementTokenBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Unix(1750000000, 0).UTC()
	delegable := authz.TokenScopes()
	validScope := delegable[0].String()
	validExpiry := now.Add(time.Hour)

	allProjectsIDs, err := validManagementTokenAt(managementTokenInput{
		Name:      "global-token",
		Scopes:    []string{validScope},
		ExpiresAt: &validExpiry,
	}, now)
	if err != nil {
		t.Fatalf("nil ProjectIDs rejected: %v", err)
	}
	if allProjectsIDs == nil || len(allProjectsIDs) != 0 {
		t.Fatalf("nil ProjectIDs returned %#v, want non-nil empty slice", allProjectsIDs)
	}

	maxExpiry := now.Add(366 * 24 * time.Hour)
	if _, err := validManagementTokenAt(managementTokenInput{
		Name:      "max-expiry",
		Scopes:    []string{validScope},
		ExpiresAt: &maxExpiry,
	}, now); err != nil {
		t.Fatalf("exact 366-day expiry rejected: %v", err)
	}

	hundredProjects := make([]string, 100)
	for i := range hundredProjects {
		hundredProjects[i] = fmt.Sprintf("01900000-0000-7000-8000-%012x", i+1)
	}
	if got, err := validManagementTokenAt(managementTokenInput{
		Name:       "hundred-projects",
		Scopes:     []string{validScope},
		ExpiresAt:  &validExpiry,
		ProjectIDs: &hundredProjects,
	}, now); err != nil || len(got) != 100 {
		t.Fatalf("100 projects: len=%d err=%v, want 100 nil", len(got), err)
	}

	tooManyProjects := append(slices.Clone(hundredProjects), "01900000-0000-7000-8000-00000000ffff")
	emptyProjects := []string{}
	duplicateCaseProjects := []string{
		"01900000-0000-7000-8000-00000000000A",
		"01900000-0000-7000-8000-00000000000a",
	}
	invalidUUIDProjects := []string{"not-a-uuid"}
	pastExpiry := now.Add(-time.Second)
	exactNow := now
	overMaxExpiry := now.Add(366*24*time.Hour + time.Second)

	cases := []struct {
		name  string
		input managementTokenInput
	}{
		{"blank name", managementTokenInput{Name: "   ", Scopes: []string{validScope}, ExpiresAt: &validExpiry}},
		{"long name", managementTokenInput{Name: strings.Repeat("a", 101), Scopes: []string{validScope}, ExpiresAt: &validExpiry}},
		{"empty scopes", managementTokenInput{Name: "t", Scopes: []string{}, ExpiresAt: &validExpiry}},
		{"duplicate scopes", managementTokenInput{Name: "t", Scopes: []string{validScope, validScope}, ExpiresAt: &validExpiry}},
		{"unknown scope", managementTokenInput{Name: "t", Scopes: []string{"nonexistent"}, ExpiresAt: &validExpiry}},
		{"non-delegable scope", managementTokenInput{Name: "t", Scopes: []string{authz.ManageSessions.String()}, ExpiresAt: &validExpiry}},
		{"empty project_ids", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &validExpiry, ProjectIDs: &emptyProjects}},
		{"too many projects", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &validExpiry, ProjectIDs: &tooManyProjects}},
		{"duplicate project UUIDs ignoring case", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &validExpiry, ProjectIDs: &duplicateCaseProjects}},
		{"invalid project UUID", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &validExpiry, ProjectIDs: &invalidUUIDProjects}},
		{"nil expiry", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: nil}},
		{"past expiry", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &pastExpiry}},
		{"expiry equal to now", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &exactNow}},
		{"expiry beyond 366 days", managementTokenInput{Name: "t", Scopes: []string{validScope}, ExpiresAt: &overMaxExpiry}},
	}
	for _, tc := range cases {
		if _, err := validManagementTokenAt(tc.input, now); err == nil {
			t.Errorf("%s: accepted invalid input", tc.name)
		}
	}
}

func TestCreatedManagementTokenAndRecoveryJSONContract(t *testing.T) {
	t.Parallel()
	now := time.Unix(1750000000, 0).UTC()
	created := createdManagementToken{
		managementTokenRecord: managementTokenRecord{
			ID:             "01900000-0000-7000-8000-000000000001",
			LookupID:       "lookup",
			Name:           "deploy",
			Scopes:         []string{"keys"},
			AllProjects:    true,
			ProjectIDs:     []string{},
			CreatedBy:      "01900000-0000-7000-8000-000000000002",
			CreatedByEmail: "owner@example.com",
			ETag:           "01900000-0000-7000-8000-000000000003",
			ExpiresAt:      now.Add(time.Hour),
			RevokedAt:      nil,
			CreatedAt:      now,
		},
		Secret: "olpm_lookup_secret",
	}
	raw, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal createdManagementToken: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal createdManagementToken: %v", err)
	}
	if payload["secret"] != "olpm_lookup_secret" || payload["id"] != created.ID {
		t.Fatalf("unexpected flattened payload: %v", payload)
	}
	if val, present := payload["revoked_at"]; !present || val != nil {
		t.Fatalf("revoked_at = (%v, present=%v), want (nil, true)", val, present)
	}
	if projects, ok := payload["project_ids"].([]any); !ok || len(projects) != 0 {
		t.Fatalf("project_ids = %#v, want empty array", payload["project_ids"])
	}

	recoveryRaw, err := json.Marshal(RecoveryResult{
		ID:                "01900000-0000-7000-8000-000000000002",
		Email:             "owner@example.com",
		PasswordRecovered: true,
		MFAReset:          false,
	})
	if err != nil {
		t.Fatalf("marshal RecoveryResult: %v", err)
	}
	var recovery map[string]any
	if err := json.Unmarshal(recoveryRaw, &recovery); err != nil {
		t.Fatalf("unmarshal RecoveryResult: %v", err)
	}
	if recovery["id"] != "01900000-0000-7000-8000-000000000002" ||
		recovery["email"] != "owner@example.com" ||
		recovery["password_recovered"] != true ||
		recovery["mfa_reset"] != false {
		t.Fatalf("unexpected RecoveryResult JSON: %v", recovery)
	}
}
