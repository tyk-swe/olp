//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/operatorcli"
	"github.com/tyk-swe/olp/sdk/management"
)

func TestManagementCLIConfigurationRecoversCommittedLostResponse(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	document := map[string]any{
		"api_version": "openllmproxy.dev/config/v1",
		"projects":    []any{map[string]any{"name": "CLI recovered project"}},
	}
	scopes := []string{}
	for _, scope := range access.TokenScopes() {
		scopes = append(scopes, scope.String())
	}
	token, secret := createToken(h, owner, "cli-lost-response", scopes)
	var lost atomic.Bool
	var applyCalls atomic.Int32
	var committedReply atomic.Value
	target, err := url.Parse(h.HTTP.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.URL.Path != "/api/v1/configuration/apply" {
			return nil
		}
		applyCalls.Add(1)
		if response.StatusCode == http.StatusOK && lost.CompareAndSwap(false, true) {
			body, err := io.ReadAll(response.Body)
			if err != nil {
				return err
			}
			_ = response.Body.Close()
			committedReply.Store(bytes.Clone(body))
			// The actual HTTP management handler has committed its mutation and
			// receipt. Flush one byte, then abort the declared-length body.
			response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body[:1]), cliLostResponseReader{}))
			response.ContentLength = int64(len(body))
			response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	dir := t.TempDir()
	tokenFile, documentFile, planFile := filepath.Join(dir, "token"), filepath.Join(dir, "document.json"), filepath.Join(dir, "plan.json")
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{tokenFile: []byte(secret), documentFile: encoded} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	runner := operatorcli.Runner{Out: &output, Err: io.Discard, Getenv: func(name string) string {
		if name == "OLP_MANAGEMENT_URL" {
			return server.URL
		}
		if name == "OLP_MANAGEMENT_TOKEN_FILE" {
			return tokenFile
		}
		return ""
	}}
	if err := runner.Run(t.Context(), []string{"config", "plan", "--file", documentFile, "--output", planFile}); err != nil {
		t.Fatal(err)
	}
	args := []string{"config", "apply", "--plan-file", planFile, "--idempotency-key", "cli-recover-committed"}
	if err := runner.Run(t.Context(), args); err == nil || !lost.Load() || applyCalls.Load() != 1 {
		t.Fatalf("first apply must commit with lost response: calls=%d lost=%v err=%v", applyCalls.Load(), lost.Load(), err)
	}
	var promoted int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.projects WHERE name='CLI recovered project'").Scan(&promoted); err != nil || promoted != 1 {
		t.Fatalf("real mutation did not commit: promoted=%d err=%v", promoted, err)
	}
	// An unrelated later write must not prevent replaying this original receipt.
	createProject(h, owner, "CLI intervening project")
	if err := runner.Run(t.Context(), args); err != nil {
		t.Fatalf("same saved plan/key cannot recover its committed receipt: %v", err)
	}
	if applyCalls.Load() != 2 || !bytes.Equal(bytes.TrimSpace(output.Bytes()), bytes.TrimSpace(committedReply.Load().([]byte))) {
		t.Fatalf("recovery changed original reply or dispatched extra applies: calls=%d", applyCalls.Load())
	}
	var applied int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='configuration.apply'").Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("original mutation must occur once: applied=%d err=%v", applied, err)
	}
	current := h.want(owner, "GET", "/api/v1/management-tokens/"+token["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v1/management-tokens/"+token["id"].(string)+"/revoke", nil, withMatch(current, idem("cli-revoke-replay-authority")), 200)
	err = runner.Run(t.Context(), args)
	var denied *management.APIError
	if !errors.As(err, &denied) || denied.Status != http.StatusUnauthorized {
		t.Fatalf("revoked current token recovered receipt: %v", err)
	}
}

type cliLostResponseReader struct{}

func (cliLostResponseReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
