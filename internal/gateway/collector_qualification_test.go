//go:build integration && collectors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/tests/fixtures"
	"go.opentelemetry.io/otel/trace"
)

type qualificationCollector struct {
	name, endpoint, query, authorization string
	headers                              map[string]string
}

func TestGenAICollectorQualification(t *testing.T) {
	required := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required for explicitly selected collector qualification", name)
		}
		return value
	}
	langfuse := strings.TrimRight(required("OLP_QUALIFY_LANGFUSE_URL"), "/")
	phoenix := strings.TrimRight(required("OLP_QUALIFY_PHOENIX_URL"), "/")
	authorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(required("QA_LF_PUBLIC_KEY")+":"+required("QA_LF_SECRET_KEY")))
	for _, collector := range []qualificationCollector{
		{name: "langfuse", endpoint: langfuse + "/api/public/otel/v1/traces", query: langfuse + "/api/public/observations", authorization: authorization, headers: map[string]string{"Authorization": authorization}},
		{name: "phoenix", endpoint: phoenix + "/v1/traces", query: phoenix + "/v1/projects/default/spans", headers: map[string]string{}},
	} {
		for _, streaming := range []bool{false, true} {
			mode := "unary"
			if streaming {
				mode = "streaming"
			}
			t.Run(collector.name+"/"+mode, func(t *testing.T) {
				qualifyCollector(t, collector, streaming)
			})
		}
	}
}

func qualifyCollector(t *testing.T, collector qualificationCollector, streaming bool) {
	headers, err := json.Marshal(collector.headers)
	if err != nil {
		t.Fatal(err)
	}
	headersPath := filepath.Join(t.TempDir(), "headers.json")
	if err := os.WriteFile(headersPath, headers, 0600); err != nil {
		t.Fatal(err)
	}
	handle, err := telemetry.Install(telemetry.Config{
		Endpoint: collector.endpoint, HeadersFile: headersPath, SampleRatio: 1,
		Mode: "gateway", Version: "qualification",
	})
	if err != nil {
		t.Fatalf("install exporter: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = handle.Shutdown(ctx)
	})
	identities := make(chan trace.SpanContext, 1)
	finished := make(chan struct{})
	h := newHarnessWrapped(t, Config{}, func(next http.Handler) http.Handler {
		captureIdentity := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identities <- trace.SpanContextFromContext(r.Context())
			next.ServeHTTP(w, r)
		})
		handler := telemetry.AdmittedRequest(telemetry.RequestConfig{InstallationID: "olp-m5-qualification"}, handle.Tracer(), "openai", captureIdentity)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(finished)
			handler.ServeHTTP(w, r)
		})
	})
	inputSentinel := "olp-collector-input-" + uuid.NewString()
	outputSentinel := "olp-collector-output-" + uuid.NewString()
	responseModel, inputTokens, outputTokens := modelA, int64(2), int64(3)
	var respond http.HandlerFunc
	if streaming {
		data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte("héllo 🌍"), []byte(outputSentinel))
		responseModel, inputTokens, outputTokens = "gpt-fixture", 3, 2
		respond = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(data)
		}
	} else {
		respond = completion(modelA, outputSentinel)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			respond(w, r)
		}
	})
	body, err := json.Marshal(map[string]any{
		"model": routeSlug, "messages": []map[string]string{{"role": "user", "content": inputSentinel}},
		"stream": streaming,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, nil)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("gateway response status %d", response.StatusCode)
	}
	reply, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Contains(reply, []byte(outputSentinel)) {
		t.Fatalf("gateway response mismatch: %v", err)
	}
	h.sink.await(t, 1)
	var identity trace.SpanContext
	select {
	case identity = <-identities:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway trace identity was not recorded")
	}
	if !identity.IsValid() {
		t.Fatal("gateway trace identity is invalid")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not finish its request span")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	err = handle.Shutdown(ctx)
	cancel()
	if err != nil {
		t.Fatalf("flush actual OTLP exporter: %v", err)
	}
	mode := "unary"
	if streaming {
		mode = "streaming"
	}
	artifactPrefix := collector.name + "-" + mode
	saveQualificationArtifact(t, artifactPrefix+"-gateway.json", mustQualificationJSON(t, map[string]any{
		"fixture": true, "trace_id": identity.TraceID().String(), "span_id": identity.SpanID().String(),
		"requested_model": routeSlug, "response_model": responseModel,
		"input_tokens": inputTokens, "output_tokens": outputTokens,
		"streaming": streaming, "gateway_status": response.StatusCode,
	}))
	data := awaitCollectorReadback(t, collector, identity.TraceID().String())
	for _, forbidden := range []string{inputSentinel, outputSentinel, secretA, fullKey, collector.authorization} {
		if forbidden != "" && bytes.Contains(data, []byte(forbidden)) {
			t.Fatal("collector received prohibited content or credentials")
		}
	}
	saveQualificationArtifact(t, artifactPrefix+"-readback.json", data)
	if collector.name == "langfuse" {
		assertLangfuseReadback(t, data, identity, responseModel, inputTokens, outputTokens)
	} else {
		assertPhoenixReadback(t, data, identity, responseModel, inputTokens, outputTokens)
	}
}

func awaitCollectorReadback(t *testing.T, collector qualificationCollector, traceID string) []byte {
	t.Helper()
	endpoint, err := url.Parse(collector.query)
	if err != nil {
		t.Fatal(err)
	}
	values := endpoint.Query()
	values.Set("limit", "10")
	if collector.name == "langfuse" {
		values.Set("traceId", traceID)
	} else {
		values.Set("trace_id", traceID)
	}
	endpoint.RawQuery = values.Encode()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if collector.authorization != "" {
			request.Header.Set("Authorization", collector.authorization)
		}
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			if response.StatusCode == 401 || response.StatusCode == 403 {
				t.Fatal("qualification collector rejected fixture authentication")
			}
			if response.StatusCode == http.StatusOK && readErr == nil {
				var envelope struct {
					Data []json.RawMessage `json:"data"`
				}
				if json.Unmarshal(body, &envelope) == nil && len(envelope.Data) >= 2 {
					return body
				}
			}
		}
		select {
		case <-t.Context().Done():
			t.Fatal("qualification cancelled before collector readback")
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("actual collector did not expose both gateway request and attempt spans")
	return nil
}

func assertLangfuseReadback(t *testing.T, data []byte, identity trace.SpanContext, responseModel string, input, output int64) {
	t.Helper()
	var response struct {
		Data []struct {
			Name, Type, Model string
			ID                string
			TraceID           string           `json:"traceId"`
			UsageDetails      map[string]int64 `json:"usageDetails"`
			Latency           float64
			Parent            *string `json:"parentObservationId"`
		}
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, observation := range response.Data {
		if observation.Name != "request" && observation.Name != "attempt" {
			continue
		}
		if observation.Type != "GENERATION" || observation.Model != responseModel ||
			observation.TraceID != identity.TraceID().String() ||
			observation.UsageDetails["input"] != input || observation.UsageDetails["output"] != output ||
			observation.Latency <= 0 {
			t.Fatalf("Langfuse did not display expected GenAI metadata for %s", observation.Name)
		}
		if observation.Name == "request" && observation.ID != identity.SpanID().String() ||
			observation.Name == "attempt" && (observation.Parent == nil || *observation.Parent != identity.SpanID().String()) {
			t.Fatal("Langfuse did not preserve actual gateway span identity and parenting")
		}
		found[observation.Name] = true
	}
	if !found["request"] || !found["attempt"] {
		t.Fatal("Langfuse did not display both request and attempt observations")
	}
}

func assertPhoenixReadback(t *testing.T, data []byte, identity trace.SpanContext, responseModel string, input, output int64) {
	t.Helper()
	var response struct {
		Data []struct {
			Name       string
			SpanKind   string         `json:"span_kind"`
			Attributes map[string]any `json:"attributes"`
			StartTime  time.Time      `json:"start_time"`
			EndTime    time.Time      `json:"end_time"`
			ParentID   *string        `json:"parent_id"`
			Context    struct {
				TraceID string `json:"trace_id"`
				SpanID  string `json:"span_id"`
			}
		}
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, span := range response.Data {
		if span.Name != "request" && span.Name != "attempt" {
			continue
		}
		if span.SpanKind != "LLM" || span.Attributes["llm.model_name"] != routeSlug ||
			span.Context.TraceID != identity.TraceID().String() ||
			span.Attributes["llm.token_count.prompt"] != float64(input) ||
			span.Attributes["llm.token_count.completion"] != float64(output) ||
			!span.EndTime.After(span.StartTime) {
			t.Fatalf("Phoenix did not display expected GenAI metadata for %s", span.Name)
		}
		if span.Name == "request" && span.Context.SpanID != identity.SpanID().String() ||
			span.Name == "attempt" && (span.ParentID == nil || *span.ParentID != identity.SpanID().String()) {
			t.Fatal("Phoenix did not preserve actual gateway span identity and parenting")
		}
		if model, ok := span.Attributes["gen_ai.response.model"]; ok && model != responseModel {
			t.Fatalf("Phoenix changed observed response model for %s", span.Name)
		}
		found[span.Name] = true
	}
	if !found["request"] || !found["attempt"] {
		t.Fatal("Phoenix did not display both request and attempt spans")
	}
}

func mustQualificationJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func saveQualificationArtifact(t *testing.T, name string, data []byte) {
	t.Helper()
	directory := os.Getenv("OLP_COLLECTOR_ARTIFACT_DIR")
	if directory == "" {
		return
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("OLP_COLLECTOR_ARTIFACT_DIR must be absolute")
	}
	if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
		t.Fatal(fmt.Errorf("save collector artifact: %w", err))
	}
}
