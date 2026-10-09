package access

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/secrets"
)

func TestEndUserDigestSeparatesProjectsAndInstallations(t *testing.T) {
	auth := secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), "installation-a")
	other := secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), "installation-b")
	projectA, projectB := "project-a", "project-b"
	digest := DigestEndUser(auth, &projectA, "customer-123")
	if len(digest) != 64 || digest != DigestEndUser(auth, &projectA, "customer-123") {
		t.Fatal("digest must be stable, encoded SHA-256")
	}
	for _, different := range []string{
		DigestEndUser(auth, &projectB, "customer-123"),
		DigestEndUser(auth, nil, "customer-123"),
		DigestEndUser(auth, &projectA, "customer-124"),
		DigestEndUser(other, &projectA, "customer-123"),
	} {
		if different == digest {
			t.Fatal("different identity boundaries must not correlate")
		}
	}
}

func TestEndUserIdentifiersAreBoundedMachineTokens(t *testing.T) {
	for _, value := range []string{"user-42", "Tenant:Customer_1.2", strings.Repeat("a", 128)} {
		if !ValidEndUserIdentifier(value) {
			t.Fatalf("refused machine token %q", value)
		}
	}
	for _, value := range []string{"", " user", "user@example.com", "end user", "\nuser", "用户", strings.Repeat("a", 129)} {
		if ValidEndUserIdentifier(value) {
			t.Fatalf("accepted invalid token %q", value)
		}
	}
}

func TestEstablishedEndUserSessionsFollowCurrentPolicies(t *testing.T) {
	digest := strings.Repeat("a", 64)
	authority := Authority{}
	if !authority.AllowsEndUser("") {
		t.Fatal("unconfigured identity changed admission")
	}
	authority.ProjectEndUserPolicy = &EndUserPolicy{}
	if authority.AllowsEndUser("") || !authority.AllowsEndUser(digest) {
		t.Fatal("new policy must distinguish unidentified sessions")
	}
	authority.ProjectEndUserPolicy.Blocked = []string{digest}
	if authority.AllowsEndUser(digest) {
		t.Fatal("project block was ignored")
	}
	authority.ProjectEndUserPolicy = nil
	authority.Policy.EndUserPolicy = &EndUserPolicy{Blocked: []string{digest}}
	if authority.AllowsEndUser(digest) || !authority.AllowsEndUser(strings.Repeat("b", 64)) {
		t.Fatal("key block applied to the wrong identity")
	}
}
