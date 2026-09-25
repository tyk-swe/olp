//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStrictBackgroundResponseStreamRecoversAfterReaderLoss(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	fixture.resps["resp-up-1"]["status"] = "completed"
	fixture.resps["resp-up-1"]["usage"] = map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key, other := stateKey(t, h, owner, slug, true), stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[],\"native\":{\"big\":9007199254740993,\"zero\":-0}}}\n\n")
	fixture.holdCreated.Store(true)
	fixture.respGetStream.Store("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"exact\",\"obfuscation\":\"native-padding\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"" + vendorModel + "\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10},\"native\":{\"big\":9007199254740993,\"zero\":-0}}}\n\n")
	beforeCreates := fixture.respCreates.Load()
	ctx, cancel := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/v1/responses",
		strings.NewReader(`{"model":"`+slug+`","input":"native background","store":true,"background":true,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("strict background stream: %d %s", response.StatusCode, body)
	}
	reader := bufio.NewReader(response.Body)
	var first bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		first.WriteString(line)
		if line == "\n" {
			break
		}
	}
	var created struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(first.Bytes(), []byte("event: response.created\ndata: "))), &created); err != nil {
		t.Fatal(err)
	}
	local := created.Response.ID
	if !strings.HasPrefix(local, "strict_response_") || bytes.Contains(first.Bytes(), []byte("resp-up-1")) ||
		!bytes.Contains(first.Bytes(), []byte(`"big":9007199254740993,"zero":-0`)) || fixture.respCreates.Load() != beforeCreates+1 {
		t.Fatalf("accepted stream exposed wrong identity/source or duplicate work: %s", first.Bytes())
	}
	cancel()
	response.Body.Close()
	h.HTTP.Close()
	restarted := newAccessHarnessOn(t, h.Pool, h.DBURL)
	restarted.refresh()
	before := fixture.dials.Load()
	if status, _, _ := restarted.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true&starting_after=0", other, nil, nil); status != http.StatusNotFound || fixture.dials.Load() != before {
		t.Fatalf("another key read the retained response: status=%d dispatches=%d", status, fixture.dials.Load()-before)
	}
	query := "?stream=true&starting_after=0&include%5B%5D=reasoning.encrypted_content&include_obfuscation=false"
	status, resumed, _ := restarted.gatewayRaw(http.MethodGet, "/v1/responses/"+local+query, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(resumed, []byte(`"obfuscation":"native-padding"`)) ||
		!bytes.Contains(resumed, []byte(`"big":9007199254740993,"zero":-0`)) ||
		!bytes.Contains(resumed, []byte(`"id":"`+local+`"`)) || bytes.Contains(resumed, []byte(`"id":"resp-up-1"`)) ||
		fixture.respCreates.Load() != beforeCreates+1 {
		t.Fatalf("restarted stream did not resume one native result: status=%d body=%s", status, resumed)
	}
	captured := fixture.lastRespQuery.Load().(string)
	parsed, err := url.ParseQuery(captured)
	if err != nil || parsed.Get("stream") != "true" || parsed.Get("starting_after") != "0" || parsed.Get("include[]") != "reasoning.encrypted_content" || parsed.Get("include_obfuscation") != "false" {
		t.Fatalf("provider retrieval query changed: %q %v", captured, err)
	}
	status, unary, _ := restarted.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(unary, []byte(`"id":"`+local+`"`)) || fixture.respCreates.Load() != beforeCreates+1 {
		t.Fatalf("unary recovery retried inference: status=%d body=%s", status, unary)
	}
	var attempts, input, output int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&attempts, &input, &output); err != nil || attempts != 1 || input != 4 || output != 6 {
		t.Fatalf("background reader-loss usage duplicated/lost: attempts=%d input=%d output=%d err=%v", attempts, input, output, err)
	}
	var providerID, credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT p.id::text,s.credential_id::text FROM olp_go.providers p JOIN olp_go.provider_slots s ON s.provider_id=p.id AND s.is_default`).Scan(&providerID, &credentialID); err != nil {
		t.Fatal(err)
	}
	detail := restarted.want(owner, "GET", "/api/v3/providers/"+providerID, nil, nil, http.StatusOK)
	restarted.want(owner, "POST", "/api/v3/providers/"+providerID+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	restarted.refresh()
	before = fixture.dials.Load()
	if status, _, _ := restarted.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true", key, nil, nil); status != http.StatusConflict || fixture.dials.Load() != before {
		t.Fatalf("revoked historical credential reached provider: status=%d dispatches=%d", status, fixture.dials.Load()-before)
	}
}

func TestStrictBackgroundResponseFailedTerminalIsVisibleAndSettled(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	escapedVendorSecret := strings.ReplaceAll(vendorSecret, "-", `\u002d`)
	status, created, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"background failure","background":true,"store":true}`),
		map[string]string{"Content-Type": "application/json"})
	local, ok := jsonStringField(created, "id")
	if status != http.StatusOK || !ok || !strings.HasPrefix(local, "strict_response_") {
		t.Fatalf("pending strict response: %d %s", status, created)
	}
	fixture.respGetStream.Store("event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":7,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"failed\",\"model\":\"" + vendorModel + "\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10},\"error\":{\"code\":\"server_error\",\"message\":\"provider echoed " + escapedVendorSecret + "\"}}}\n\n")
	before := fixture.dials.Load()
	status, failed, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true&starting_after=6", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(failed, []byte(`"type":"response.failed"`)) ||
		!bytes.Contains(failed, []byte(`"id":"`+local+`"`)) || bytes.Contains(failed, []byte("resp-up-1")) ||
		bytes.Contains(failed, []byte(vendorSecret)) || bytes.Contains(failed, []byte(escapedVendorSecret)) || !bytes.Contains(failed, []byte("[REDACTED]")) || fixture.dials.Load() != before+1 {
		t.Fatalf("native failed terminal was lost or exposed a credential: status=%d body=%s", status, failed)
	}
	terminal := sink.last()
	if terminal.Outcome != "failure" || terminal.ErrorClass != "upstream_response_failed" || !terminal.Committed || len(terminal.Attempts) != 1 || terminal.Attempts[0].Class != "upstream_server" {
		t.Fatalf("failed terminal was claimed successful: %+v", terminal)
	}
	for range 2 {
		status, _, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true&starting_after=6", key, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("repeated failed terminal stream: %d", status)
		}
	}
	var attempts, input, output int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&attempts, &input, &output); err != nil || attempts != 1 || input != 4 || output != 6 {
		t.Fatalf("failed terminal usage duplicated/lost: attempts=%d input=%d output=%d err=%v", attempts, input, output, err)
	}
}

// A retained replay whose provider terminal is response.incomplete is a
// successful native outcome: the gateway delivers the event with its empty
// visible output, reasoning item and incomplete_details unchanged, records no
// fault, reconciles the retained state to "incomplete" and settles the native
// usage exactly once.
func TestStrictBackgroundResponseIncompleteTerminalIsANativeOutcome(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	status, created, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"incomplete work","background":true,"store":true}`),
		map[string]string{"Content-Type": "application/json"})
	local, ok := jsonStringField(created, "id")
	if status != http.StatusOK || !ok || !strings.HasPrefix(local, "strict_response_") {
		t.Fatalf("pending strict response: %d %s", status, created)
	}
	fixture.respGetStream.Store("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"sequence_number\":7,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"incomplete\",\"model\":\"" + vendorModel + "\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[]}],\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n")
	status, body, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true&starting_after=6", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"type":"response.incomplete"`)) ||
		!bytes.Contains(body, []byte(`"incomplete_details":{"reason":"max_output_tokens"}`)) ||
		!bytes.Contains(body, []byte(`"type":"reasoning"`)) ||
		!bytes.Contains(body, []byte(`"id":"`+local+`"`)) || bytes.Contains(body, []byte("resp-up-1")) ||
		bytes.Contains(body, []byte("event: error")) {
		t.Fatalf("native incomplete terminal was relabeled or gained a synthetic error: status=%d body=%s", status, body)
	}
	terminal := sink.last()
	if terminal.Outcome != "success" || terminal.ErrorClass != "" || !terminal.Committed || len(terminal.Attempts) != 1 ||
		terminal.Attempts[0].Class != "success" || terminal.Attempts[0].NativeStatus != "incomplete" ||
		terminal.Attempts[0].FaultOrigin != "" {
		t.Fatalf("native incomplete terminal was not recorded as its own outcome: %+v", terminal)
	}
	var state string
	if err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp_go.provider_resources WHERE kind='strict_response' AND upstream_id='resp-up-1'`).Scan(&state); err != nil || state != "incomplete" {
		t.Fatalf("retained state did not reconcile to the native terminal: %q %v", state, err)
	}
	var attempts, input, output int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&attempts, &input, &output); err != nil || attempts != 1 || input != 4 || output != 6 {
		t.Fatalf("incomplete terminal usage duplicated/lost: attempts=%d input=%d output=%d err=%v", attempts, input, output, err)
	}
}

func TestStrictBackgroundResponsePinnedJavaScriptSDKRecovery(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[]}}\n\n")
	fixture.holdCreated.Store(true)
	fixture.respGetStream.Store("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"" + vendorModel + "\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n")
	before := fixture.respCreates.Load()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tests/sdk-smoke/node_modules/openai")); err != nil {
		t.Fatal("pinned OpenAI JavaScript SDK is missing; run pnpm install --frozen-lockfile")
	}
	cmd := exec.CommandContext(t.Context(), "node", "tests/sdk-smoke/strict-background-stream.mjs")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "OLP_RESPONSES_BASE="+h.HTTP.URL, "OLP_RESPONSES_ROUTE="+slug, "OLP_RESPONSES_KEY="+key)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned Responses background SDK: %v\n%s", err, output)
	}
	var result struct {
		ID      string `json:"id"`
		Resumed bool   `json:"resumed"`
	}
	if json.Unmarshal(bytes.TrimSpace(output), &result) != nil || !result.Resumed || !strings.HasPrefix(result.ID, "strict_response_") || fixture.respCreates.Load() != before+1 {
		t.Fatalf("SDK did not resume one accepted work: %s", output)
	}
	captured := fixture.lastRespQuery.Load().(string)
	query, err := url.ParseQuery(captured)
	if err != nil || query.Get("stream") != "true" || query.Get("starting_after") != "0" || query.Get("include[]") != "reasoning.encrypted_content" || query.Get("include_obfuscation") != "false" {
		t.Fatalf("SDK native retrieval query changed: %q %v", captured, err)
	}
}

func TestStrictBackgroundResponsePendingCancelAndExpiry(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "queued"
	fixture.resps["resp-up-1"]["output"] = []any{}
	fixture.resps["resp-up-1"]["usage"] = nil
	status, body, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"queued work","background":true,"store":true}`),
		map[string]string{"Content-Type": "application/json"})
	local, ok := jsonStringField(body, "id")
	if status != http.StatusOK || !ok || !strings.HasPrefix(local, "strict_response_") {
		t.Fatalf("strict queued submission: %d %s", status, body)
	}
	for _, state := range []string{"queued", "in_progress"} {
		fixture.resps["resp-up-1"]["status"] = state
		status, body, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
		if status != http.StatusOK || !bytes.Contains(body, []byte(`"status":"`+state+`"`)) ||
			!bytes.Contains(body, []byte(`"id":"`+local+`"`)) || bytes.Contains(body, []byte("resp-up-1")) {
			t.Fatalf("pending native state lost during retrieval: %d %s", status, body)
		}
	}
	status, body, _ = h.gatewayRaw(http.MethodPost, "/v1/responses/"+local+"/cancel", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"status":"cancelled"`)) ||
		!bytes.Contains(body, []byte(`"id":"`+local+`"`)) {
		t.Fatalf("cancelled native state lost: %d %s", status, body)
	}
	var count int64
	var charge string
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(max(charge_status),'') FROM olp_go.attempt_usage_facts
 WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&count, &charge); err != nil || count != 1 || charge != "billing_uncertain" {
		t.Fatalf("terminal missing usage was not recorded conservatively: count=%d charge=%q err=%v", count, charge, err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE kind='strict_response' AND route_slug=$1`, slug); err != nil {
		t.Fatal(err)
	}
	before := fixture.dials.Load()
	if status, _, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil); status != http.StatusNotFound || fixture.dials.Load() != before {
		t.Fatalf("expired response reached provider: status=%d dispatches=%d", status, fixture.dials.Load()-before)
	}
}

func TestStrictResponseParentIdentitySurvivesParentExpiry(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	create := func(source string) string {
		t.Helper()
		status, body, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key, strings.NewReader(source), map[string]string{"Content-Type": "application/json"})
		local, ok := jsonStringField(body, "id")
		if status != http.StatusOK || !ok || !strings.HasPrefix(local, "strict_response_") {
			t.Fatalf("strict response submission: %d %s", status, body)
		}
		return local
	}
	first := create(`{"model":"` + slug + `","input":"first","background":true,"store":true}`)
	fixture.respID.Store("resp-up-2")
	secondSource := `{"model":"` + slug + `","input":"second","previous_response_id":"` + first + `","background":true,"store":true}`
	status, body, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key, strings.NewReader(secondSource), map[string]string{"Content-Type": "application/json"})
	second, ok := jsonStringField(body, "id")
	if status != http.StatusOK || !ok || !strings.HasPrefix(second, "strict_response_") || !bytes.Contains(body, []byte(`"previous_response_id":"`+first+`"`)) || bytes.Contains(body, []byte(`"previous_response_id":"resp-up-1"`)) {
		t.Fatalf("strict child leaked its native parent: %d %s", status, body)
	}
	request := fixture.lastReq.Load().(map[string]any)
	if request["previous_response_id"] != "resp-up-1" {
		t.Fatalf("native provider did not receive its owned parent: %v", request["previous_response_id"])
	}
	child := map[string]any{}
	for name, value := range fixture.resps["resp-up-1"] {
		child[name] = value
	}
	child["id"], child["previous_response_id"] = "resp-up-2", "resp-up-1"
	fixture.resps["resp-up-2"] = child
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE kind='strict_response' AND upstream_id='resp-up-1'`); err != nil {
		t.Fatal(err)
	}
	status, body, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+second, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"previous_response_id":"`+first+`"`)) || bytes.Contains(body, []byte(`"previous_response_id":"resp-up-1"`)) {
		t.Fatalf("child lost the encrypted parent identity after parent expiry: %d %s", status, body)
	}
	child["previous_response_id"] = "forged-upstream-parent"
	status, body, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+second, key, nil, nil)
	if status != http.StatusBadGateway || !bytes.Contains(body, []byte("fidelity_protocol_violation")) || bytes.Contains(body, []byte("forged-upstream-parent")) {
		t.Fatalf("provider changed retained parent without refusal: %d %s", status, body)
	}
}

func TestStrictBackgroundPostStreamFailedTerminalSettlesBeforeDelivery(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	escapedVendorSecret := strings.ReplaceAll(vendorSecret, "-", `\u002d`)
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[]}}\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":1,\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"failed\",\"model\":\"" + vendorModel + "\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10},\"error\":{\"code\":\"server_error\",\"message\":\"provider echoed " + escapedVendorSecret + "\"}}}\n\n")
	status, stream, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"failed in stream","background":true,"store":true,"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !bytes.Contains(stream, []byte(`"type":"response.created"`)) ||
		!bytes.Contains(stream, []byte(`"type":"response.failed"`)) || !bytes.Contains(stream, []byte(`"id":"strict_response_`)) ||
		bytes.Contains(stream, []byte(`"id":"resp-up-1"`)) || bytes.Contains(stream, []byte(vendorSecret)) || bytes.Contains(stream, []byte(escapedVendorSecret)) {
		var state string
		_ = h.Pool.QueryRow(t.Context(), `SELECT state FROM olp_go.provider_resources WHERE kind='strict_response' AND upstream_id='resp-up-1'`).Scan(&state)
		var facts int64
		_ = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&facts)
		t.Fatalf("failed native POST terminal was not safely delivered: status=%d state=%q facts=%d stream=%s", status, state, facts, stream)
	}
	terminal := sink.last()
	if terminal.Outcome != "failure" || terminal.Committed != true || len(terminal.Attempts) != 1 || !terminal.Attempts[0].ResponseUsageDeferred {
		t.Fatalf("failed POST stream claimed success or duplicated metering: %+v", terminal)
	}
	var count, input, output int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&count, &input, &output); err != nil || count != 1 || input != 4 || output != 6 {
		t.Fatalf("failed POST metering not committed before terminal: count=%d input=%d output=%d err=%v", count, input, output, err)
	}
	// A credential in an unknown decoded member name cannot be safely renamed.
	// Refuse that frame, but retain the provider's already-observed terminal
	// state and usage before closing the client stream.
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-up-2\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[]}}\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-up-2\",\"object\":\"response\",\"status\":\"failed\",\"model\":\"" + vendorModel + "\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2},\"error\":{\"code\":\"server_error\",\"message\":\"safe\",\"" + escapedVendorSecret + "\":true}}}\n\n")
	status, stream, _ = h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"unsafe failed frame","background":true,"store":true,"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !bytes.Contains(stream, []byte(`"type":"response.created"`)) ||
		bytes.Contains(stream, []byte(`"type":"response.failed"`)) || bytes.Contains(stream, []byte(escapedVendorSecret)) || bytes.Contains(stream, []byte(vendorSecret)) {
		t.Fatalf("unsafe failed frame was delivered: status=%d stream=%s", status, stream)
	}
	var state string
	if err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp_go.provider_resources WHERE kind='strict_response' AND upstream_id='resp-up-2'`).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("unsafe failed frame lost accepted terminal state: state=%q err=%v", state, err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&count, &input, &output); err != nil || count != 2 || input != 5 || output != 8 {
		t.Fatalf("unsafe failed frame lost observed billing: count=%d input=%d output=%d err=%v", count, input, output, err)
	}
}

func TestStrictBackgroundFailedUnaryResourceRedactsEscapedCredential(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	status, created, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"pending failure","background":true,"store":true}`),
		map[string]string{"Content-Type": "application/json"})
	local, ok := jsonStringField(created, "id")
	if status != http.StatusOK || !ok || !strings.HasPrefix(local, "strict_response_") {
		t.Fatalf("strict pending failure setup: %d %s", status, created)
	}
	escapedSecret := strings.ReplaceAll(vendorSecret, "-", `\u002d`)
	failed := `{"id":"resp-up-1","object":"response","status":"failed","model":"` + vendorModel + `","output":[],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10},"error":{"code":"server_error","message":"provider echoed ` + escapedSecret + `"},"native":{"big":9007199254740993,"zero":-0}}`
	fixture.respFetchRaw.Store(failed)
	fixture.respCancelRaw.Store(failed)
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/v1/responses/" + local},
		{http.MethodPost, "/v1/responses/" + local + "/cancel"},
	} {
		status, body, _ := h.gatewayRaw(call.method, call.path, key, nil, nil)
		if status != http.StatusOK || !bytes.Contains(body, []byte(`"id":"`+local+`"`)) ||
			!bytes.Contains(body, []byte(`"message":"provider echoed [REDACTED]"`)) ||
			!bytes.Contains(body, []byte(`"big":9007199254740993,"zero":-0`)) ||
			bytes.Contains(body, []byte(vendorSecret)) || bytes.Contains(body, []byte(escapedSecret)) {
			t.Fatalf("unary failed resource exposed credential or changed native bytes: status=%d body=%s", status, body)
		}
	}
	unsafeKey := strings.Replace(failed, `"native"`, `"`+escapedSecret+`"`, 1)
	fixture.respFetchRaw.Store(unsafeKey)
	status, body, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
	if status != http.StatusBadGateway || !bytes.Contains(body, []byte("fidelity_protocol_violation")) || bytes.Contains(body, []byte(vendorSecret)) || bytes.Contains(body, []byte(escapedSecret)) {
		t.Fatalf("credential-bearing native member name was exposed: %d %s", status, body)
	}
	var count, input, output int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&count, &input, &output); err != nil || count != 1 || input != 4 || output != 6 {
		t.Fatalf("failed resource reconciliation changed by redaction refusal: count=%d input=%d output=%d err=%v", count, input, output, err)
	}
}

func TestStrictBackgroundResourceHTTPErrorRedactsEscapedCredential(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	status, created, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"pending HTTP failure","background":true,"store":true}`),
		map[string]string{"Content-Type": "application/json"})
	local, ok := jsonStringField(created, "id")
	if status != http.StatusOK || !ok {
		t.Fatalf("strict response setup: %d %s", status, created)
	}
	escapedSecret := strings.ReplaceAll(vendorSecret, "-", `\u002d`)
	fixture.respFetchRaw.Store(`{"error":{"code":"bad_request","message":"provider echoed ` + escapedSecret + `"}}`)
	fixture.respFetchStatus.Store(http.StatusBadRequest)
	status, body, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("[REDACTED]")) ||
		bytes.Contains(body, []byte(vendorSecret)) || bytes.Contains(body, []byte(escapedSecret)) {
		t.Fatalf("upstream HTTP error exposed decoded provider credential: %d %s", status, body)
	}
}

func TestStrictBackgroundStreamErrorKeepsAcceptedWorkRecoverable(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	escapedSecret := strings.ReplaceAll(vendorSecret, "-", `\u002d`)
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[]}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"provider_interrupted\",\"message\":\"provider echoed " + escapedSecret + "\"}}\n\n")
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	before := fixture.respCreates.Load()
	status, stream, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"background provider error","background":true,"store":true,"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	var local string
	for line := range strings.SplitSeq(string(stream), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Response.ID != "" {
			local = event.Response.ID
			break
		}
	}
	if status != http.StatusOK || !strings.HasPrefix(local, "strict_response_") ||
		!bytes.Contains(stream, []byte(`"type":"error"`)) || !bytes.Contains(stream, []byte("[REDACTED]")) ||
		bytes.Contains(stream, []byte(vendorSecret)) || bytes.Contains(stream, []byte(escapedSecret)) || fixture.respCreates.Load() != before+1 {
		t.Fatalf("provider stream error was not safely delivered: status=%d stream=%s", status, stream)
	}
	var state string
	var pending bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT state, metadata ? 'pending_usage' FROM olp_go.provider_resources
 WHERE kind='strict_response' AND upstream_id='resp-up-1'`).Scan(&state, &pending); err != nil || state != "in_progress" || !pending {
		t.Fatalf("stream error falsely terminated accepted work: state=%q pending=%t err=%v", state, pending, err)
	}
	fixture.resps["resp-up-1"]["status"] = "completed"
	fixture.resps["resp-up-1"]["usage"] = map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}
	status, result, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(result, []byte(`"status":"completed"`)) ||
		!bytes.Contains(result, []byte(`"id":"`+local+`"`)) || fixture.respCreates.Load() != before+1 {
		t.Fatalf("accepted work could not finish after native stream error: %d %s", status, result)
	}
	var count, input, output int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&count, &input, &output); err != nil || count != 1 || input != 4 || output != 6 {
		t.Fatalf("accepted work after stream error was billed twice or lost: count=%d input=%d output=%d err=%v", count, input, output, err)
	}
}

func TestStrictBackgroundStreamRejectsResponseIDDrift(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.respPostStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-drift-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[]}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-drift-2\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"" + vendorModel + "\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")
	status, stream, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"identity drift","background":true,"store":true,"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !bytes.Contains(stream, []byte(`"type":"response.created"`)) ||
		bytes.Contains(stream, []byte(`"type":"response.completed"`)) || !bytes.Contains(stream, []byte(`"code":"provider_protocol_error"`)) {
		t.Fatalf("provider changed response ID without an incomplete result: %d %s", status, stream)
	}
	var first, second int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE upstream_id='resp-drift-1'),
 count(*) FILTER (WHERE upstream_id='resp-drift-2') FROM olp_go.provider_resources WHERE kind='strict_response'`).Scan(&first, &second); err != nil || first != 1 || second != 0 {
		t.Fatalf("ID drift installed another accepted resource: first=%d second=%d err=%v", first, second, err)
	}
}
