//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/runtime"
	"log/slog"
	"net/http"
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

func TestOverlapExpiresWithoutAnotherAuthorityRefresh(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "short overlap"}, idem("create"), 201)
	path := "/api/v1/api-keys/" + key["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	for _, value := range []any{-1, 86401, 1.5, nil} {
		headers := etagHeader(detail)
		headers["Idempotency-Key"] = "invalid"
		h.want(owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": value}, headers, 422)
	}
	headers := etagHeader(detail)
	headers["Idempotency-Key"] = "rotate"
	result := h.want(owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": 2}, headers, 200)
	h.refresh()
	if _, err := h.Runtime.Authenticate(key["secret"].(string)); err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339Nano, result["overlap_expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(until) + time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if _, err = h.Runtime.Authenticate(key["secret"].(string)); !errors.Is(err, runtime.ErrInvalidKey) {
		t.Fatalf("expired cached overlap: %v", err)
	}
	if _, err = h.Runtime.Authenticate(result["secret"].(string)); err != nil {
		t.Fatal("current secret expired with overlap", err)
	}
	var stored []byte
	if err = h.Pool.QueryRow(context.Background(), "SELECT jsonb_agg(to_jsonb(o)) FROM olp.api_key_overlaps o").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stored) || strings.Contains(string(stored), key["secret"].(string)) || strings.Contains(string(stored), result["secret"].(string)) {
		t.Fatal("plaintext secret in overlap state")
	}
}

func TestRotationBoundsPriorSecretsAndValidatesReminderPolicy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "bounded versions"}, idem("create"), 201)
	path := "/api/v1/api-keys/" + key["id"].(string)
	var firstDeadline any
	for i := 0; i < 8; i++ {
		detail := h.want(owner, "GET", path, nil, nil, 200)
		headers := etagHeader(detail)
		headers["Idempotency-Key"] = fmt.Sprintf("rotate-%d", i)
		result := h.want(owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": 60}, headers, 200)
		if i == 0 {
			firstDeadline = result["overlap_expires_at"]
		}
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	active := detail["active_overlaps"].([]any)
	if len(active) != 8 {
		t.Fatalf("active versions: %d", len(active))
	}
	firstTime, _ := time.Parse(time.RFC3339Nano, firstDeadline.(string))
	storedTime, _ := time.Parse(time.RFC3339Nano, active[0].(map[string]any)["expires_at"].(string))
	if len(active) != 8 || !storedTime.Equal(firstTime) {
		t.Fatal("rotation extended a prior deadline")
	}
	headers := etagHeader(detail)
	headers["Idempotency-Key"] = "too-many"
	h.want(owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": 60}, headers, 409)
	for _, days := range []any{0, 3651, 1.5} {
		h.want(owner, "PATCH", path, map[string]any{"rotation_interval_days": days}, etagHeader(detail), 422)
	}
	h.want(owner, "PATCH", path, map[string]any{"rotation_interval_days": 1}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	if detail["rotation_due_at"] == nil {
		t.Fatal("missing due rotation")
	}
	h.want(owner, "PATCH", path, map[string]any{"rotation_interval_days": nil}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	if detail["rotation_due_at"] != nil {
		t.Fatal("cleared reminder retained its due date")
	}
	headers = etagHeader(detail)
	headers["Idempotency-Key"] = "revoke-all"
	h.want(owner, "POST", path+"/revoke", nil, headers, 200)
	h.refresh()
	if status, err := endUserChat(t.Context(), h, key["secret"].(string), "unused", "unused"); err != nil || status != 401 {
		t.Fatalf("revoked overlap still authenticated: %d %v", status, err)
	}
}

func TestRotationKeepsInflightConcurrencyAndSettledTokens(t *testing.T) {
	t.Run("concurrency", func(t *testing.T) {
		f := glSeedIn(t, "rotation-concurrency", glPrice{})
		id, old := f.key("held", map[string]any{"max_concurrency": 1})
		f.vendor.delay.Store(int64(30 * time.Second))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", f.h.HTTP.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+old)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
		path := "/api/v1/api-keys/" + id
		detail := f.h.want(f.owner, "GET", path, nil, nil, 200)
		headers := etagHeader(detail)
		headers["Idempotency-Key"] = "held-rotation"
		next := f.h.want(f.owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": 60}, headers, 200)["secret"].(string)
		f.h.refresh()
		if status, err := endUserChat(t.Context(), f.h, next, routeSlug, "unused"); err != nil || status != 429 {
			t.Fatalf("rotation bypassed in-flight concurrency: %d %v", status, err)
		}
		cancel()
		response.Body.Close()
		glEventually(t, "old-secret lease release", func() bool {
			status, err := endUserChat(t.Context(), f.h, next, routeSlug, "unused")
			return err == nil && status == 200
		})
	})
	t.Run("tokens", func(t *testing.T) {
		f := glSeedIn(t, "rotation-tokens", glPrice{})
		id, old := f.key("tokens", map[string]any{"tokens_per_minute": 20})
		limSettleInMinute(t, f.valkey, 20*time.Second)
		if status, err := endUserChat(t.Context(), f.h, old, routeSlug, "unused"); err != nil || status != 200 {
			t.Fatalf("initial request: %d %v", status, err)
		}
		rate := f.namespace + ":{" + strings.Split(old, "_")[1] + "}:rate"
		glEventually(t, "token reconciliation", func() bool {
			value, err := f.valkey.Do(t.Context(), "HGET", rate, "tpm")
			return err == nil && value == "17"
		})
		path := "/api/v1/api-keys/" + id
		detail := f.h.want(f.owner, "GET", path, nil, nil, 200)
		headers := etagHeader(detail)
		headers["Idempotency-Key"] = "token-rotation"
		next := f.h.want(f.owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": 60}, headers, 200)["secret"].(string)
		f.h.refresh()
		if status, err := endUserChat(t.Context(), f.h, next, routeSlug, "unused"); err != nil || status != 429 {
			t.Fatalf("rotation reset token spend: %d %v", status, err)
		}
	})
}

func TestRotationOverlapIsBoundedByCurrentKeyExpiry(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "expiry-bound", "expires_at": expiry}, idem("new"), 201)
	path := "/api/v1/api-keys/" + key["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	headers := etagHeader(detail)
	headers["Idempotency-Key"] = "bounded-expiry"
	result := h.want(owner, "POST", path+"/rotate", map[string]any{"overlap_seconds": 86400}, headers, 200)
	until, err := time.Parse(time.RFC3339, result["overlap_expires_at"].(string))
	if err != nil || until.Format(time.RFC3339) != expiry {
		t.Fatal("overlap exceeded key expiry")
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	shorter := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	h.want(owner, "PATCH", path, map[string]any{"expires_at": shorter.Format(time.RFC3339)}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	until, err = time.Parse(time.RFC3339, detail["active_overlaps"].([]any)[0].(map[string]any)["expires_at"].(string))
	if err != nil || !until.Equal(shorter) {
		t.Fatal("overlap metadata ignored current expiry")
	}
}
