package access

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/workload"
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

func TestConsumerScopesWorkForKeysAndWorkloadMappings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
		valid  bool
	}{
		{name: "all consumer scopes", scopes: ConsumerScopes(), valid: true},
		{name: "unknown scope", scopes: []string{"unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := keyInput{Name: "key", KeyPolicy: KeyPolicy{Scopes: tc.scopes, AllowedRoutes: []string{}}}
			issuer := workload.Config{
				Name: "workload", Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys",
				Audiences: []string{"gateway"}, Algorithms: []string{"RS256"}, MaxLifetimeSeconds: 600,
				DisabledKeyIDs: []string{},
				Mappings: []workload.Mapping{{Name: "mapping", Match: map[string]string{"/sub": "workload"},
					ProjectID: "00000000-0000-4000-8000-000000000001", LimitTemplate: "build",
					RouteGroups: []string{"generation"}, Scopes: tc.scopes}},
			}
			if err := validateKey(key, false); (err == nil) != tc.valid {
				t.Fatalf("key validation = %v, valid = %t", err, tc.valid)
			}
			if err := issuer.Validate(); (err == nil) != tc.valid {
				t.Fatalf("workload validation = %v, valid = %t", err, tc.valid)
			}
		})
	}
}
