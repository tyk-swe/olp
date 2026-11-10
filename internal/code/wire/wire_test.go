package wire_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/code/wire"
)

// Fixture shape per https://opencode.ai/v2/docs/console/api/inference/ (fetched 2026-11-05).
const openCodeUsage = `"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"cost":0.00412,"opencode":{"billing_source":"go","list_cost":0.00412,"allowance":0.0021,"tokens":{"input":1200,"output":300,"reasoning":120,"cache_read":8000,"cache_write_5m":0,"cache_write_1h":0}}}`

func TestAllowanceChargeAcrossProtocols(t *testing.T) {
	chatObs := wire.NewChat().Unary([]byte(`{"choices":[{"finish_reason":"stop"}],` + openCodeUsage + `}`))
	if !chatObs.Terminal || !chatObs.Successful || chatObs.AllowanceCharge == nil {
		t.Fatalf("chat observation = %+v", chatObs)
	}
	var charge map[string]any
	if err := json.Unmarshal(chatObs.AllowanceCharge, &charge); err != nil || charge["billing_source"] != "go" || charge["allowance"] != 0.0021 || charge["cost"] != 0.00412 {
		t.Fatalf("chat charge = %v (%v)", charge, err)
	}
	privateObs := wire.NewChat().Unary([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"cost":0.004,"opencode":{"billing_source":"go","prompt":"private prompt","tokens":{"input":3,"output":2,"extra":"private"}}}}`))
	if strings.Contains(string(privateObs.AllowanceCharge), "private") {
		t.Fatalf("charge leaked private content: %s", privateObs.AllowanceCharge)
	}
	msgObs := wire.NewMessages().Unary([]byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":300,"cost":0.00412,"opencode":{"billing_source":"go","allowance":0.0021}}}`))
	if !msgObs.Terminal || msgObs.AllowanceCharge == nil {
		t.Fatalf("messages observation = %+v", msgObs)
	}
	respObs := wire.ObserveResponses([]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1200,"output_tokens":300,"total_tokens":1500,"cost":0.00412,"opencode":{"billing_source":"go","allowance":0.0021}}}}`), false)
	if !respObs.Terminal || !respObs.Successful || respObs.AllowanceCharge == nil {
		t.Fatalf("responses observation = %+v", respObs)
	}
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), bare error object without type.
	bareErr := []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at 2026-11-05T12:00:00Z"}}`)
	if got := wire.NewMessages().Unary(bareErr); string(got.RejectionBody) != string(bareErr) || !got.Terminal {
		t.Fatalf("messages bare error = %+v", got)
	}
}

func TestCodexAllowanceHeadersAndEvents(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "25")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-At", "1794000000")
	headers.Set("X-Codex-Five_hour-Used-Percent", "50")
	allowance := wire.CodexAllowance(headers, now)
	if allowance == nil || len(allowance.Windows) != 1 || allowance.Windows[0].Window != "primary" || allowance.Windows[0].RemainingPercent != 75 {
		t.Fatalf("header allowance = %+v", allowance)
	}
	stream := []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
	if got := wire.ObserveResponses(stream, false); string(got.RejectionBody) != string(stream) || got.Status != 429 {
		t.Fatalf("stream rejection = %+v", got)
	}
}
