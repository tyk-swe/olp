package access

import (
	"testing"
	"time"
)

func TestConsumerResourceDenyMatrix(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	resource := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	now := time.Now()
	a := Authority{ProjectID: &project, Policy: KeyPolicy{Scopes: []string{"inference", "tools", "agents", "traces_write"}, AllowedRoutes: []string{"*"}, AllowedRouteGroups: []string{"all"}, AllowedToolsets: []string{resource}}}
	if !a.AllowsResource("tools", resource, project, now) || !a.AllowsResource("traces_write", "", project, now) {
		t.Fatal("explicit consumer authority refused")
	}
	for _, input := range [][3]string{{"agents", resource, project}, {"tools", "different", project}, {"tools", resource, "different"}, {"inference", resource, project}, {"traces_write", resource, project}, {"delegate", "", project}} {
		if a.AllowsResource(input[0], input[1], input[2], now) {
			t.Fatalf("unexpected authority for %v", input)
		}
	}
	a.Policy.AllowedToolsets = nil
	if a.AllowsResource("tools", resource, project, now) {
		t.Fatal("routes or scope granted an empty toolset allowlist")
	}
	a.Policy.AllowedToolsets = []string{resource}
	a.Policy.Scopes = []string{"inference"}
	if a.AllowsResource("tools", resource, project, now) {
		t.Fatal("allowlist bypassed independent scope")
	}
	a.Policy.Scopes = []string{"tools"}
	a.RevokedAt = &now
	if a.AllowsResource("tools", resource, project, now) {
		t.Fatal("revoked key accepted")
	}
	a.RevokedAt = nil
	a.ExpiresAt = &now
	if a.AllowsResource("tools", resource, project, now) {
		t.Fatal("expired key accepted")
	}
	a.ExpiresAt = nil
	a.ProjectID = nil
	if a.AllowsResource("tools", resource, project, now) {
		t.Fatal("installation key acquired project authority")
	}
	copy := ConsumerScopes()
	copy[0] = "mutated"
	if ConsumerScopes()[0] != "inference" {
		t.Fatal("scope registry is mutable")
	}
}
