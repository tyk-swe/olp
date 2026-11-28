//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/platform/workerhealth"
	"github.com/tyk-swe/olp/tests/harness"
)

type service struct{ *harness.Service }

func (s *service) withT(t *testing.T) *service {
	return &service{s.Service.WithT(t)}
}

func startService(t *testing.T) *service {
	t.Helper()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Service test")
	return &service{harness.StartProcess(t, pool, env)}
}

func staleETag() map[string]string {
	return map[string]string{"If-Match": `"` + uuid.NewString() + `"`}
}

func sameJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (s *service) expectStatus(c *harness.Caller, method, path string, body any, headers map[string]string, want int) {
	s.T.Helper()
	if status, raw := s.Do(c, method, path, body, headers); status != want {
		s.T.Fatalf("%s %s: %s, want %d", method, path, describe(status, raw), want)
	}
}

func (s *service) assignProjectMember(owner *harness.Caller, email, userRole, projectID, projectRole string) (*harness.Caller, string) {
	s.T.Helper()
	invited := s.Invite(owner, email, userRole)
	profile := s.Want(invited, "GET", "/api/v1/profile", nil, nil, http.StatusOK)
	userID := profile["id"].(string)
	s.Want(owner, "PATCH", "/api/v1/users/"+userID, map[string]any{"access_scope": "assigned"}, harness.ETag(profile), http.StatusOK)
	if projectID != "" {
		project := s.Want(owner, "GET", "/api/v1/projects/"+projectID, nil, nil, http.StatusOK)
		s.Want(owner, "PUT", "/api/v1/projects/"+projectID+"/members/"+userID, map[string]any{"role": projectRole}, harness.ETag(project), http.StatusOK)
	}
	return s.Login(email), userID
}

func auditActions(t *testing.T, s *service, resource string) []string {
	t.Helper()
	rows, err := s.Pool.Query(t.Context(), `SELECT action COLLATE "C" FROM olp.audit WHERE resource_id::text=$1 ORDER BY 1`, resource)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return actions
}

func TestStartupPortCollision(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Service test")
	occupied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer occupied.Close()
	attempts := 0
	addresses := func(t *testing.T) (string, string) {
		attempts++
		public, private := harness.FreeAddresses(t)
		if attempts == 1 {
			private = strings.TrimPrefix(occupied.URL, "http://")
		}
		return public, private
	}
	s := harness.StartProcessAttempt(t, pool, env, addresses, time.Now().Add(harness.StartupLimit), 3)
	if attempts < 2 || attempts > 3 {
		t.Fatalf("startup attempts = %d, want 2 or 3", attempts)
	}
	s.Owner()
}

func TestEachSplitModeAnswersOnlyItsOwnPlaneAndStopsCleanly(t *testing.T) {
	t.Parallel()
	status := func(t *testing.T, s *harness.Service, path string) int {
		t.Helper()
		response, err := http.Get(s.Origin + path)
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	for mode, want := range map[string]struct{ management, inference int }{
		"control": {management: http.StatusOK, inference: http.StatusNotFound},
		"gateway": {management: http.StatusNotFound, inference: http.StatusUnauthorized},
		"worker":  {},
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			pool, databaseURL := harness.Database(t)
			env := harness.NewEnvironment(t, databaseURL, "Split mode test")
			env.Mode = mode
			s := harness.StartProcess(t, pool, env)
			if got := status(t, s, "/api/v1/setup/status"); got != want.management {
				t.Fatalf("management answered %d, want %d", got, want.management)
			}
			if got := status(t, s, "/v1/models"); got != want.inference {
				t.Fatalf("inference answered %d, want %d", got, want.inference)
			}
		})
	}
}

func TestEveryWorkerTaskTheDeploymentCanServeRecordsASuccess(t *testing.T) {
	t.Parallel()
	for name, deployment := range map[string]struct {
		mode          string
		withoutValkey bool
	}{
		"worker with Valkey": {mode: "worker"},
		"all without Valkey": {mode: "all", withoutValkey: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool, databaseURL := harness.Database(t)
			env := harness.NewEnvironment(t, databaseURL, "Worker plane test")
			env.Mode = deployment.mode
			if deployment.withoutValkey {
				env.Values["OLP_VALKEY_URL"] = ""
			}
			s := harness.StartProcess(t, pool, env)
			s.Eventually("a success from every worker task", 20*time.Second, func() (bool, string) {
				rows, err := pool.Query(t.Context(), `SELECT task FROM olp.worker_task_health WHERE successes_total>0`)
				if err != nil {
					return false, err.Error()
				}
				succeeded, err := pgx.CollectRows(rows, pgx.RowTo[workerhealth.Task])
				if err != nil {
					return false, err.Error()
				}
				missing := slices.DeleteFunc(slices.Clone(workerhealth.Tasks[:]), func(task workerhealth.Task) bool {
					return slices.Contains(succeeded, task) || deployment.withoutValkey && task.Regional()
				})
				return len(missing) == 0, fmt.Sprintf("none from %v", missing)
			})
		})
	}
}

func describe(status int, raw []byte) string { return fmt.Sprintf("status %d: %s", status, raw) }
