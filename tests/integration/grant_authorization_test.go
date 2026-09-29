//go:build integration

package integration_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

// Upstream steps run outside the installation lock. Losing project reach
// during one must prevent its enrollment or credential from being stored.
func TestGrantEnrollmentRechecksProjectReachAfterUpstreamSteps(t *testing.T) {
	for _, step := range []string{"start", "continue", "poll"} {
		t.Run(step, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			operator := h.invite(owner, "operator@example.com", "operator")
			profile := h.want(operator, "GET", "/api/v1/profile", nil, nil, 200)
			userID := profile["id"].(string)
			h.want(owner, "PATCH", "/api/v1/users/"+userID, map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
			projectID := createProject(h, owner, "Grant account")
			addMember(h, owner, projectID, userID, "manager")
			operator = login(h, "operator@example.com")

			authority := testutil.NewOAuthServer(t)
			handler := authority.Config.Handler
			authority.Close()
			var revoke atomic.Bool
			boundary := "/userinfo"
			if step == "start" {
				boundary = "/device/code"
			}
			authority.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == boundary && revoke.Swap(false) {
					if _, err := h.Pool.Exec(r.Context(), "DELETE FROM olp.project_members WHERE project_id=$1 AND user_id=$2", projectID, userID); err != nil {
						t.Error(err)
						http.Error(w, "project change failed", http.StatusInternalServerError)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(authority.Close)
			digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
			profileID := "reference-device-chat"
			if step == "continue" {
				profileID = "reference-grant-chat"
			}
			created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
				"name": "Scoped grant account", "model": vendorModel, "project_id": projectID,
				"configuration": map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": profileID, "profile_revision": digest},
			}, idem("scoped-grant"), 201)
			path := "/api/v1/providers/" + created["id"].(string)
			detail := h.want(owner, "GET", path, nil, nil, 200)
			if step == "start" {
				revoke.Store(true)
				h.want(operator, "POST", path+"/grant-enrollments", nil, etagHeader(detail), 404)
			} else {
				enrollment := startGrantEnrollment(t, h, operator, path)
				if step == "continue" {
					callback := signIn(t, enrollment)
					revoke.Store(true)
					continueGrantEnrollment(h, operator, path, enrollment, callback.String(), 404)
				} else {
					device := enrollment["device"].(map[string]any)
					testutil.DecideDevice(t, device["verification_url"].(string), device["user_code"].(string), "approve")
					pollDue(t, h, enrollment)
					revoke.Store(true)
					pollGrantEnrollment(h, operator, path, enrollment, 404)
				}
			}
			if revoke.Load() {
				t.Fatal("the upstream step never reached the authority change")
			}
			var credentials int
			providerID := strings.TrimPrefix(path, "/api/v1/providers/")
			if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.provider_credentials WHERE provider_id=$1", providerID).Scan(&credentials); err != nil || credentials != 0 {
				t.Fatalf("stored %d credentials after project reach was removed: %v", credentials, err)
			}
			if step == "start" {
				var enrollments int
				if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.grant_enrollments WHERE provider_id=$1", providerID).Scan(&enrollments); err != nil || enrollments != 0 {
					t.Fatalf("stored %d enrollments after project reach was removed: %v", enrollments, err)
				}
			}
		})
	}
}

func TestGrantEnrollmentByManagementTokenKeepsItsActorAndOwnership(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
	path := grantProvider(t, h, owner, digest, nil)
	created, token := createToken(h, owner, "grant operator", []string{"configure"})
	tokenID := created["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	enrollment := h.machineWant(token, "POST", path+"/grant-enrollments", nil, etagHeader(detail), 201)
	callback := signIn(t, enrollment)
	// Its creator is a different principal from the token that enrolled.
	continueGrantEnrollment(h, owner, path, enrollment, callback.String(), 404)
	completed := h.machineWant(token, "POST", path+"/grant-enrollments/"+enrollment["id"].(string)+"/continue", map[string]any{"input": callback.String()}, nil, 201)
	var actor string
	if err := h.Pool.QueryRow(t.Context(), "SELECT actor_management_token_id::text FROM olp.audit WHERE action='provider.grant.enroll' AND resource_id=$1 AND outcome='success'", completed["credential_id"]).Scan(&actor); err != nil || actor != tokenID {
		t.Fatalf("grant enrollment actor %q, want token %q: %v", actor, tokenID, err)
	}
}
