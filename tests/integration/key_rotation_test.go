//go:build integration

package integration_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestOverlappingRotationSharesCurrentPolicyAndCounters(t *testing.T) {
	f := glSeedIn(t, "rotation-overlap", glPrice{})
	id, old := f.key("rotate", map[string]any{"requests_per_minute": 2, "rotation_interval_days": 30})
	path := "/api/v1/api-keys/" + id
	second := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL)
	second.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), f.namespace), nil, slog.New(slog.DiscardHandler))
	limSettleInMinute(t, f.valkey, 20*time.Second)
	if status, _, _ := f.chat(old); status != 200 {
		t.Fatal(status)
	}
	detail := f.h.want(f.owner, "GET", path, nil, nil, 200)
	headers := etagHeader(detail)
	headers["Idempotency-Key"] = "overlap"
	body := map[string]any{"overlap_seconds": 60}
	rotated := f.h.want(f.owner, "POST", path+"/rotate", body, headers, 200)
	replay := f.h.want(f.owner, "POST", path+"/rotate", body, headers, 200)
	if rotated["secret"] != replay["secret"] || rotated["overlap_expires_at"] != replay["overlap_expires_at"] {
		t.Fatal("replay changed the secret or overlap deadline")
	}
	next := rotated["secret"].(string)
	f.h.refresh()
	second.refresh()
	a, err := f.h.Runtime.Authenticate(old)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Runtime.Authenticate(next)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.LimitsLookup() != b.LimitsLookup() || a.LimitsLookup() != strings.Split(old, "_")[1] {
		t.Fatal("rotation changed admission identity")
	}
	if status, err := endUserChat(t.Context(), second, next, routeSlug, "unused"); err != nil || status != 200 {
		t.Fatalf("replacement: %d %v", status, err)
	}
	if status, _, _ := f.chat(old); status != 429 {
		t.Fatalf("old secret reset counters: %d", status)
	}
	detail = f.h.want(f.owner, "GET", path, nil, nil, 200)
	if len(detail["active_overlaps"].([]any)) != 1 || detail["rotation_due_at"] == nil {
		t.Fatal("missing lifecycle metadata")
	}
	f.h.want(f.owner, "PATCH", path, map[string]any{"scopes": []string{"models_read"}}, etagHeader(detail), 200)
	f.h.refresh()
	second.refresh()
	for _, secret := range []string{old, next} {
		if status, err := endUserChat(t.Context(), second, secret, routeSlug, "unused"); err != nil || status != 403 {
			t.Fatalf("overlap retained old scopes: %d %v", status, err)
		}
	}
	detail = f.h.want(f.owner, "GET", path, nil, nil, 200)
	headers = etagHeader(detail)
	headers["Idempotency-Key"] = "immediate"
	replacement := f.h.want(f.owner, "POST", path+"/rotate", nil, headers, 200)["secret"].(string)
	second.refresh()
	for _, secret := range []string{old, next} {
		if _, err = second.Runtime.Authenticate(secret); !errors.Is(err, runtime.ErrInvalidKey) {
			t.Fatalf("zero overlap retained a prior secret: %v", err)
		}
	}
	if _, err = second.Runtime.Authenticate(replacement); err != nil {
		t.Fatal(err)
	}
	detail = f.h.want(f.owner, "GET", path, nil, nil, 200)
	headers = etagHeader(detail)
	headers["Idempotency-Key"] = "revoke"
	f.h.want(f.owner, "POST", path+"/revoke", nil, headers, 200)
	second.refresh()
	if status, err := endUserChat(t.Context(), second, replacement, routeSlug, "unused"); err != nil || status != 401 {
		t.Fatalf("revoked replacement: %d %v", status, err)
	}
}
