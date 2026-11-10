package vendor_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/upstream"
)

func TestRegistryValidatesAndResolvesProfileSignals(t *testing.T) {
	if err := vendor.Validate(); err != nil {
		t.Fatalf("vendor registry invalid: %v", err)
	}
	for _, profile := range []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile} {
		v, ok := vendor.ForConnection("plugin", "grant", profile)
		if !ok || v.Adapter != code.AdapterMiniMaxPlan {
			t.Fatalf("profile %q = %+v, %v", profile, v, ok)
		}
		signal, ok := vendor.AllowanceSignalForProfile("plugin", "grant", profile)
		if !ok || signal.Signal != vendor.AllowanceSignalRemainsUnqualified || signal.ActiveQualified {
			t.Fatalf("profile %q signal = %+v, %v", profile, signal, ok)
		}
	}
	codex, ok := vendor.AllowanceSignalForProfile("plugin", "grant", codexauth.ProfileID)
	if !ok || codex.Signal != vendor.AllowanceSignalProven || codex.ActiveQualified {
		t.Fatalf("codex signal = %+v, %v", codex, ok)
	}
	kimi, _ := vendor.Lookup(code.AdapterKimiCode)
	qwen, _ := vendor.Lookup(code.AdapterQwenTokenPlan)
	zai, _ := vendor.Lookup(code.AdapterZAICoding)
	if !kimi.ExtraUsageAllowed || !qwen.SupportsCycleAnchor(codeplans.QwenTokenPlanProfile) || !zai.SupportsResearch(codeplans.ZAIProfile) || zai.SupportsResearch(codeplans.BigModelProfile) {
		t.Fatal("vendor declarative capabilities mismatched")
	}
}

func TestClassifyAndInitialRejectionAcrossVendors(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)

	codex, _ := vendor.Lookup(code.AdapterCodex)
	codexEndpoint, _ := codex.Endpoint("responses")
	codexBody := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + strconv.FormatInt(reset.Unix(), 10) + `}}`)
	got, ok := codex.Classify(codexEndpoint, 429, codexBody, http.Header{}, now)
	if !ok || got.Class != code.CooldownQuota || got.Scope != code.ScopeCredential || !got.RetryAt.Equal(reset) || !got.NotExecuted {
		t.Fatalf("codex quota = %+v, %v", got, ok)
	}
	if rej, ok := codex.InitialRejection(codexEndpoint, 429, codexBody, http.Header{}, now); !ok || rej.Class != upstream.RateLimit || !rej.RetryAt.Equal(reset) {
		t.Fatalf("codex initial rejection = %+v, %v", rej, ok)
	}
	if rej, ok := codex.InitialRejection(codexEndpoint, 401, []byte(`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`), http.Header{}, now); !ok || rej.Class != upstream.Credential {
		t.Fatalf("codex 401 initial rejection = %+v, %v", rej, ok)
	}
	if capClass, ok := codex.Classify(codexEndpoint, 429, []byte(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`), http.Header{}, now); !ok || capClass.Class != code.CooldownCapacity || capClass.Scope != code.ScopeModel || capClass.NotExecuted {
		t.Fatalf("codex capacity = %+v, %v", capClass, ok)
	}

	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), codes 1308, 1309, 1311.
	glm, _ := vendor.Lookup(code.AdapterZAICoding)
	glmEndpoint, _ := glm.Endpoint("v1/chat/completions")
	glmQuota := []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at ` + reset.Format(time.RFC3339) + `"}}`)
	if got, ok := glm.Classify(glmEndpoint, 429, glmQuota, http.Header{}, now); !ok || got.Class != code.CooldownQuota || !got.RetryAt.Equal(reset) || got.NotExecuted {
		t.Fatalf("glm quota = %+v, %v", got, ok)
	}
	if got, ok := glm.Classify(glmEndpoint, 429, []byte(`{"error":{"code":"1309","message":"expired"}}`), http.Header{}, now); !ok || !got.SuppressFallback {
		t.Fatalf("glm expiry = %+v, %v", got, ok)
	}
	if _, ok := glm.InitialRejection(glmEndpoint, 429, glmQuota, http.Header{}, now); ok {
		t.Fatal("glm unproven quota must not authorize replay")
	}

	// Source: MoonshotAI/kimi-code@df90d90 and https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05).
	kimi, _ := vendor.Lookup(code.AdapterKimiCode)
	kimiEndpoint, _ := kimi.Endpoint("v1/chat/completions")
	kimiQuota := []byte(`{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`)
	if got, ok := kimi.Classify(kimiEndpoint, 429, kimiQuota, http.Header{}, now); !ok || got.Class != code.CooldownQuota || got.NotExecuted {
		t.Fatalf("kimi quota = %+v, %v", got, ok)
	}
	if _, ok := kimi.InitialRejection(kimiEndpoint, 429, kimiQuota, http.Header{}, now); ok {
		t.Fatal("kimi quota must not authorize replay")
	}

	// Source: MiniMax-AI/cli@38aba5f and https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05).
	minimax, _ := vendor.Lookup(code.AdapterMiniMaxPlan)
	minimaxEndpoint, _ := minimax.Endpoint("v1/chat/completions")
	if got, ok := minimax.Classify(minimaxEndpoint, 429, []byte(`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`), http.Header{}, now); !ok || got.Class != code.CooldownQuota || got.NotExecuted {
		t.Fatalf("minimax quota = %+v, %v", got, ok)
	}

	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05).
	qwen, _ := vendor.Lookup(code.AdapterQwenTokenPlan)
	qwenEndpoint, _ := qwen.Endpoint("v1/chat/completions")
	qwenQuota := []byte(`{"error":{"message":"Your token-plan quota has been exhausted.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`)
	qwenClass, ok := qwen.Classify(qwenEndpoint, 429, qwenQuota, http.Header{}, now)
	if !ok || qwenClass.Class != code.CooldownQuota || qwenClass.Envelope != vendor.QwenQuotaEnvelope || qwenClass.NotExecuted {
		t.Fatalf("qwen quota = %+v, %v", qwenClass, ok)
	}
	anchor := "2026-01-15"
	adjusted := vendor.WithCycleReset(qwenClass, &anchor, now)
	if !adjusted.RetryAt.Equal(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("qwen cycle reset = %s", adjusted.RetryAt)
	}
}
