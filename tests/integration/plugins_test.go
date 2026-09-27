//go:build integration

package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/testutil"
)

var wasm = map[string]string{"Content-Type": "application/wasm"}

func digestOf(module []byte) string {
	sum := sha256.Sum256(module)
	return hex.EncodeToString(sum[:])
}

func wantUsable(t *testing.T, h *accessHarness, digest, code string) {
	t.Helper()
	_, _, err := plugins.Usable(t.Context(), h.Pool, digest)
	if refusal, ok := errors.AsType[*plugins.Error](err); code == "" && err != nil || code != "" && (!ok || refusal.Code != code) {
		t.Fatalf("want usability %q, got %v", code, err)
	}
}

// An owner installs a plugin, reviews what it declares, approves exactly its
// declared origins and uninstalls it; every other principal only reads.
func TestOwnerInstallsApprovesAndUninstallsAPlugin(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	operator := h.invite(owner, "operator@example.com", "operator")
	viewer := h.invite(owner, "viewer@example.com", "viewer")
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "automation", "scopes": []string{"read", "configure", "access"}, "expires_at": time.Now().Add(time.Hour),
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)["secret"].(string)
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference")
	digest := digestOf(module)
	path := "/api/v1/plugins/" + digest

	h.want(nil, "POST", "/api/v1/plugins", module, wasm, 401)
	h.want(operator, "POST", "/api/v1/plugins", module, wasm, 403)
	if status, _ := h.machine(token, "POST", "/api/v1/plugins", nil, wasm); status != 403 {
		t.Fatalf("a management token installed a plugin: %d", status)
	}
	if code := problemCode(t, h.want(owner, "POST", "/api/v1/plugins", module, nil, 415)); code != "unsupported_media_type" {
		t.Fatalf("installed a JSON upload: %s", code)
	}

	status, installed, headers := h.request(owner, "POST", "/api/v1/plugins", module, wasm)
	if status != 201 || headers.Get("Location") != path || installed["digest"] != digest || installed["approved_at"] != nil || installed["installed_by_email"] != "owner@example.com" {
		t.Fatalf("install answered %d %v %v", status, headers, installed)
	}
	manifest := installed["manifest"].(map[string]any)
	if manifest["name"] != "reference" || !reflect.DeepEqual(manifest["origins"], []any{"https://api.example.com", "https://login.example.com"}) ||
		manifest["profiles"].([]any)[0].(map[string]any)["dialect"] != "openai-chat" || installed["abi_version"] != float64(1) || installed["size_bytes"] != float64(len(module)) {
		t.Fatalf("install recorded %v", installed)
	}
	again := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 200)
	if again["etag"] != installed["etag"] || again["installed_at"] != installed["installed_at"] {
		t.Fatalf("installing the same digest again changed it: %v", again)
	}

	for _, reader := range []*browser{viewer, operator} {
		items := h.want(reader, "GET", "/api/v1/plugins", nil, nil, 200)["items"].([]any)
		if len(items) != 1 || !reflect.DeepEqual(items[0].(map[string]any)["manifest"], manifest) {
			t.Fatalf("readers see %v", items)
		}
		h.want(reader, "GET", path, nil, nil, 200)
	}
	if status, listed := h.machine(token, "GET", "/api/v1/plugins", nil, nil); status != 200 || len(listed["items"].([]any)) != 1 {
		t.Fatalf("a read-scoped token listed %d %v", status, listed)
	}
	h.want(owner, "GET", "/api/v1/plugins/"+strings.ToUpper(digest), nil, nil, 400)
	h.want(owner, "GET", "/api/v1/plugins/"+strings.Repeat("0", 64), nil, nil, 404)

	// Until an owner approves exactly the declared origins, nothing can use it.
	wantUsable(t, h, digest, plugins.CodeNotApproved)
	h.want(viewer, "POST", path+"/approve", map[string]any{"origins": manifest["origins"]}, etagHeader(installed), 403)
	h.want(owner, "POST", path+"/approve", map[string]any{"origins": manifest["origins"]}, nil, 428)
	for _, origins := range [][]string{{"https://api.example.com"}, {"https://api.example.com", "https://login.example.com", "https://extra.example.com"}, {}} {
		mismatch := h.want(owner, "POST", path+"/approve", map[string]any{"origins": origins}, etagHeader(installed), 422)
		if problemCode(t, mismatch) != "plugin_origins_mismatch" || mismatch["errors"].(map[string]any)["origins"] == nil {
			t.Fatalf("approved %v: %v", origins, mismatch)
		}
	}
	approved := h.want(owner, "POST", path+"/approve", map[string]any{"origins": []string{"https://login.example.com", "https://api.example.com"}}, etagHeader(installed), 200)
	if approved["approved_at"] == nil || approved["approved_by_email"] != "owner@example.com" || approved["etag"] == installed["etag"] {
		t.Fatalf("approval recorded %v", approved)
	}
	if code := problemCode(t, h.want(owner, "POST", path+"/approve", map[string]any{"origins": manifest["origins"]}, etagHeader(approved), 409)); code != "plugin_approved" {
		t.Fatalf("approved twice: %s", code)
	}
	wantUsable(t, h, digest, "")
	if _, stored, _ := plugins.Usable(t.Context(), h.Pool, digest); !bytes.Equal(stored, module) {
		t.Fatal("the stored module differs from the upload")
	}

	h.want(operator, "DELETE", path, nil, etagHeader(approved), 403)
	h.want(owner, "DELETE", path, nil, nil, 428)
	h.want(owner, "DELETE", path, nil, etagHeader(installed), 412)
	h.want(owner, "DELETE", path, nil, etagHeader(approved), 204)
	h.want(owner, "GET", path, nil, nil, 404)
	wantUsable(t, h, digest, plugins.CodeNotInstalled)
	if reinstalled := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 201); reinstalled["approved_at"] != nil {
		t.Fatalf("a reinstalled plugin kept its approval: %v", reinstalled)
	}

	rows, err := h.Pool.Query(t.Context(), "SELECT a.action, a.resource_id, u.email FROM olp.audit a JOIN olp.users u ON u.id = a.actor_user_id WHERE a.resource_type='plugin' ORDER BY a.occurred_at, a.id")
	if err != nil {
		t.Fatal(err)
	}
	var trail []string
	for rows.Next() {
		var action, resource, actor string
		if err = rows.Scan(&action, &resource, &actor); err != nil {
			t.Fatal(err)
		}
		if resource != digest || actor != "owner@example.com" {
			t.Fatalf("audit attributes %s on %s to %s", action, resource, actor)
		}
		trail = append(trail, action)
	}
	if want := []string{"plugin.install", "plugin.approve", "plugin.uninstall", "plugin.install"}; !reflect.DeepEqual(trail, want) {
		t.Fatalf("audit trail %v, want %v", trail, want)
	}
}

func TestInstallRefusesPluginsOLPCannotRun(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for name, tc := range map[string]struct {
		module      []byte
		code, field string
	}{
		"another ABI version": {testutil.BuildPlugin(t, "./internal/plugins/testdata/otherabi"), plugins.CodeABIUnsupported, ""},
		"invalid manifest":    {testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour=invalid-manifest"), plugins.CodeManifestInvalid, "manifest.origins[0]"},
		"unknown dialect":     {testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour=unknown-dialect"), plugins.CodeDialectUnknown, "manifest.profiles[0].dialect"},
		"not WebAssembly":     {[]byte("#!/bin/sh\n"), plugins.CodeModuleInvalid, ""},
	} {
		t.Run(name, func(t *testing.T) {
			refusal := h.want(owner, "POST", "/api/v1/plugins", tc.module, wasm, 422)
			if problemCode(t, refusal) != tc.code {
				t.Fatalf("refused with %v", refusal)
			}
			if tc.field != "" {
				if issues, _ := refusal["errors"].(map[string]any)[tc.field].([]any); len(issues) != 1 || issues[0].(map[string]any)["code"] != tc.code {
					t.Fatalf("refusal does not locate %s: %v", tc.field, refusal)
				}
			}
		})
	}
	oversized := make([]byte, 32<<20+1)
	if code := problemCode(t, h.want(owner, "POST", "/api/v1/plugins", oversized, wasm, 413)); code != "plugin_too_large" {
		t.Fatalf("oversized upload refused with %s", code)
	}
	if items := h.want(owner, "GET", "/api/v1/plugins", nil, nil, 200)["items"].([]any); len(items) != 0 {
		t.Fatalf("refused plugins were installed: %v", items)
	}
}

// Several digests of one plugin install side by side.
func TestDigestsOfOnePluginInstallSideBySide(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	first := h.want(owner, "POST", "/api/v1/plugins", testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour=first"), wasm, 201)
	second := h.want(owner, "POST", "/api/v1/plugins", testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour=second"), wasm, 201)
	items := h.want(owner, "GET", "/api/v1/plugins", nil, nil, 200)["items"].([]any)
	if first["digest"] == second["digest"] || len(items) != 2 || items[0].(map[string]any)["digest"] != second["digest"] {
		t.Fatalf("side-by-side digests listed as %v", items)
	}
	for _, item := range items {
		if item.(map[string]any)["manifest"].(map[string]any)["name"] != "fixture" {
			t.Fatalf("listed %v", item)
		}
	}
}
