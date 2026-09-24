//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNegotiatedContinuationOfficialSDKAutomaticRetryUsesOneAcceptedWork(t *testing.T) {
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	d := barrierCorpus(t, 0)
	provider := newBarrierProvider(t, []barrierDocuments{d})
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	provider.measured.Store(true)
	baselineEvents := sink.count()
	if _, err := os.Stat(filepath.Join("..", "sdk-smoke", "node_modules", "openai")); err != nil {
		t.Fatal("official OpenAI JavaScript SDK is missing; run pnpm install --frozen-lockfile")
	}
	cmd := exec.CommandContext(t.Context(), "node", "tests/sdk-smoke/negotiated-retry.mjs")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "OLP_CONTINUATION_ORIGIN="+h.HTTP.URL, "OLP_CONTINUATION_KEY="+key, "OLP_CONTINUATION_ROUTE="+slug)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "one accepted tool stream") {
		t.Fatalf("official SDK retry did not recover: %v %s", err, output)
	}
	if provider.first.Load() != 1 || provider.next.Load() != 0 {
		t.Fatalf("SDK retry repeated accepted provider work: first=%d next=%d", provider.first.Load(), provider.next.Load())
	}
	attempts := 0
	for _, event := range sink.all()[baselineEvents:] {
		attempts += len(event.Attempts)
	}
	if attempts != 1 {
		t.Fatalf("proxy-local delivery retry created %d inference Attempts", attempts)
	}
}
