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
 FROM olp.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&attempts, &input, &output); err != nil || attempts != 1 || input != 4 || output != 6 {
		t.Fatalf("background reader-loss usage duplicated/lost: attempts=%d input=%d output=%d err=%v", attempts, input, output, err)
	}
	var providerID, credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT p.id::text,s.credential_id::text FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id AND s.is_default`).Scan(&providerID, &credentialID); err != nil {
		t.Fatal(err)
	}
	detail := restarted.want(owner, "GET", "/api/v1/providers/"+providerID, nil, nil, http.StatusOK)
	restarted.want(owner, "POST", "/api/v1/providers/"+providerID+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	restarted.refresh()
	before = fixture.dials.Load()
	if status, _, _ := restarted.gatewayRaw(http.MethodGet, "/v1/responses/"+local+"?stream=true", key, nil, nil); status != http.StatusConflict || fixture.dials.Load() != before {
		t.Fatalf("revoked historical credential reached provider: status=%d dispatches=%d", status, fixture.dials.Load()-before)
	}
}

// A credential in an unknown decoded member name cannot be safely renamed.
// Refuse that frame, but retain the provider's already-observed terminal
// state and usage before closing the client stream.
