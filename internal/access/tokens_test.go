package access

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

type stubRow struct {
	values []any
	err    error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, target := range dest {
		switch v := target.(type) {
		case *string:
			*v = r.values[i].(string)
		case *[]byte:
			*v = r.values[i].([]byte)
		case *bool:
			*v = r.values[i].(bool)
		}
	}
	return nil
}

type stubQueryer struct {
	row pgx.Row
}

func (q stubQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }
func (q stubQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func machineServer(t *testing.T) *Server {
	t.Helper()
	key, err := secrets.DecodeKey(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Origin: "https://console.test", Auth: secrets.NewAuthKey(key, "installation")}
}

func machineRequest(secret, method string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/routes", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	return r
}

func TestManagementTokenValidation(t *testing.T) {
	future := time.Now().Add(30 * 24 * time.Hour)
	valid := managementTokenInput{Name: "deploy", Scopes: []string{"read", "configure"}, ExpiresAt: &future}
	if err := validManagementToken(valid); err != nil {
		t.Fatal("rejected a valid token", err)
	}
	for name, mutate := range map[string]func(*managementTokenInput){
		"empty name": func(i *managementTokenInput) { i.Name = " " },
		"long name":  func(i *managementTokenInput) { i.Name = strings.Repeat("n", 101) },
		"no scopes":  func(i *managementTokenInput) { i.Scopes = nil },
		"scope maximum": func(i *managementTokenInput) {
			i.Scopes = []string{"read", "access_read", "access", "settings", "configure", "keys", "playground", "usage", "read"}
		},
		"duplicate":     func(i *managementTokenInput) { i.Scopes = []string{"read", "read"} },
		"unknown scope": func(i *managementTokenInput) { i.Scopes = []string{"inference"} },
		"no expiry":     func(i *managementTokenInput) { i.ExpiresAt = nil },
		"past expiry": func(i *managementTokenInput) {
			past := time.Now().Add(-time.Minute)
			i.ExpiresAt = &past
		},
		"distant expiry": func(i *managementTokenInput) {
			far := time.Now().Add(367 * 24 * time.Hour)
			i.ExpiresAt = &far
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := valid
			mutate(&input)
			if err := validManagementToken(input); err == nil {
				t.Fatal("accepted invalid token input")
			}
		})
	}
	all := valid
	all.Scopes = managementScopeNames
	if err := validManagementToken(all); err != nil {
		t.Fatal("rejected the full scope set", err)
	}
}

func TestMachinePrincipalResolution(t *testing.T) {
	s := machineServer(t)
	secret := "olpm_lookup_random"
	digest := s.Auth.Digest("management_token", secret)
	scopes, _ := json.Marshal([]string{"read", "configure"})
	live := stubQueryer{stubRow{values: []any{"token-uuid", "deploy", scopes, digest, "creator-uuid", true, true, []byte("[]")}}}

	r := machineRequest(secret, "GET")
	p, err := s.Principal(r, live, "read")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "machine" || p.ID != "token-uuid" || p.DisplayName != "deploy" || p.Creator != "creator-uuid" {
		t.Fatal("machine principal lost its token identity", p)
	}
	if p.UserID() != "creator-uuid" {
		t.Fatal("machine ownership should attribute the token creator")
	}
	if _, err = s.Principal(r, live, "usage"); err == nil {
		t.Fatal("an unscoped operation must not authorize")
	}
	projectIDs, _ := json.Marshal([]string{"p1", "p2"})
	settingsScopes, _ := json.Marshal([]string{"read", "settings"})
	projectScoped := stubQueryer{stubRow{values: []any{"token-uuid", "deploy", settingsScopes, digest, "creator-uuid", true, false, projectIDs}}}
	scoped, err := s.Principal(r, projectScoped, "read")
	if err != nil {
		t.Fatal(err)
	}
	if scoped.AllProjects || scoped.Projects["p1"] != "manager" || scoped.Projects["p2"] != "manager" {
		t.Fatal("a project-scoped token must resolve manager-equivalent projects", scoped)
	}
	if _, err = s.Principal(r, projectScoped, "settings"); err == nil {
		t.Fatal("a project-scoped token must not run installation operations")
	}
	for name, row := range map[string]pgx.Row{
		"unknown":    stubRow{err: pgx.ErrNoRows},
		"retired":    stubRow{values: []any{"token-uuid", "deploy", scopes, digest, "creator-uuid", false, true, []byte("[]")}},
		"bad digest": stubRow{values: []any{"token-uuid", "deploy", scopes, s.Auth.Digest("management_token", "olpm_lookup_other"), "creator-uuid", true, true, []byte("[]")}},
		"mangled":    stubRow{values: []any{"token-uuid", "deploy", scopes, digest, "creator-uuid", true, true, []byte("[]")}},
	} {
		t.Run(name, func(t *testing.T) {
			attempt := secret
			if name == "mangled" {
				attempt = "olpm_onlytwo"
			}
			if _, err := s.Principal(machineRequest(attempt, "GET"), stubQueryer{row}, "read"); err == nil {
				t.Fatal("an invalid machine credential authorized")
			}
		})
	}
}

func TestBearerMachineBoundary(t *testing.T) {
	for _, header := range []string{"Bearer olpm_a_b", "Bearer olpm_"} {
		if _, ok := managementBearer(machineRequestHeader(header)); !ok {
			t.Fatalf("%q did not select machine authentication", header)
		}
	}
	for _, header := range []string{"Bearer olp_a_b", "Bearer other", "bearer olpm_a_b", "olpm_a_b", "Bearer olp_a_b"} {
		if _, ok := managementBearer(machineRequestHeader(header)); ok {
			t.Fatalf("%q must not select machine authentication", header)
		}
	}
}

func machineRequestHeader(header string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", header)
	return r
}

func TestMachineBearerSkipsBrowserDefensesOnly(t *testing.T) {
	s := &Server{Origin: "https://console.test"}
	server := httptest.NewServer(s.Handle(func(r *http.Request) (Reply, error) {
		return OK(map[string]bool{"reached": true}), nil
	}))
	defer server.Close()

	send := func(headers map[string]string, cookies []*http.Cookie) int {
		r, err := http.NewRequest("POST", server.URL+"/api/v1/test", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}

	conflicting := []*http.Cookie{{Name: "__Host-olp_session", Value: "a"}, {Name: "__Host-olp_session", Value: "b"}}
	if status := send(map[string]string{"Authorization": "Bearer olpm_lookup_secret"}, conflicting); status != 200 {
		t.Fatal("an olpm bearer request must skip cookie conflict checks", status)
	}
	if status := send(map[string]string{"Authorization": "Bearer olpm_lookup_secret", "Sec-Fetch-Site": "cross-site"}, nil); status != 200 {
		t.Fatal("an olpm bearer request must skip cross-site defenses", status)
	}
	if status := send(map[string]string{"Authorization": "Bearer olpm_lookup_secret"}, nil); status != 200 {
		t.Fatal("an olpm bearer write must not need an Origin", status)
	}
	if status := send(map[string]string{"Authorization": "Bearer olp_key_secret"}, nil); status != 403 {
		t.Fatal("an inference key must not bypass origin checks", status)
	}
	if status := send(map[string]string{"Authorization": "Bearer other"}, conflicting); status != 400 {
		t.Fatal("an ordinary bearer must not skip cookie conflict checks", status)
	}
	if status := send(nil, nil); status != 403 {
		t.Fatal("a bare write must still require the console origin", status)
	}
}

func TestUsageScopeMatrix(t *testing.T) {
	for _, role := range []string{"owner", "operator", "developer", "viewer"} {
		if !Permission(role, "usage") {
			t.Fatal("usage reads must remain available to", role)
		}
	}
	if Permission("unknown", "usage") {
		t.Fatal("usage reads must reject unknown roles")
	}
}

func TestProvisioningIdentifiers(t *testing.T) {
	r := httptest.NewRequest("PUT", "/", nil)
	r.SetPathValue("source", "scim-bridge")
	r.SetPathValue("external_id", "user-42")
	if _, err := provisioningID(r, "source", "source", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioningID(r, "external_id", "external_id", 255); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", strings.Repeat("s", 101), "a/b", "a\x07b", "a\rb"} {
		r.SetPathValue("source", bad)
		if _, err := provisioningID(r, "source", "source", 100); err == nil {
			t.Fatalf("accepted provisioning source %q", bad)
		}
	}
	r.SetPathValue("external_id", strings.Repeat("e", 256))
	if _, err := provisioningID(r, "external_id", "external_id", 255); err == nil {
		t.Fatal("accepted an oversized external identifier")
	}
}
