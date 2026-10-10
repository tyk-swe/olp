package plugins

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestExecutableDirectoryErrorHidesFilesystemDetails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-deployment-path", "missing")
	s := &Management{Unconfined: &Unconfined{dir: dir}}
	_, err := s.executables(httptest.NewRequest(http.MethodGet, "/api/v1/unconfined-plugins", nil), access.Principal{})
	var problem *access.Problem
	if !errors.As(err, &problem) || problem.Status != http.StatusServiceUnavailable || problem.Code != "unconfined_plugin_dir_unreadable" {
		t.Fatalf("unexpected problem: %v", err)
	}
	reply := httptest.NewRecorder()
	access.WriteProblem(reply, err)
	if strings.Contains(reply.Body.String(), dir) || strings.Contains(reply.Body.String(), "private-deployment-path") || strings.Contains(reply.Body.String(), "no such file") {
		t.Fatalf("filesystem diagnostic disclosed: %s", reply.Body.String())
	}
}
