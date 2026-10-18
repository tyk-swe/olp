//go:build integration

package m9sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s missing; use CLIENTS=m9-go tests/clients/run.sh", k)
	}
	return v
}
func client(t *testing.T) (openai.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	return openai.NewClient(option.WithAPIKey(env(t, "OLP_CLIENTS_API_KEY")), option.WithBaseURL(env(t, "OLP_CLIENTS_OPENAI_BASE_URL")), option.WithUnsafeAllowHTTP(), option.WithMaxRetries(0)), ctx
}
func TestM9PinnedCompletions(t *testing.T) {
	c, ctx := client(t)
	// This selection shares a fixture with other suites in a full run.
	reset, _ := http.NewRequestWithContext(ctx, http.MethodDelete, env(t, "OLP_CLIENTS_UPSTREAM_URL")+"/__recorded", nil)
	response, err := http.DefaultClient.Do(reset)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("recording reset: %d", response.StatusCode)
	}
	for _, model := range []string{env(t, "OLP_CLIENTS_MODEL_OPENAI_COMPLETIONS"), env(t, "OLP_CLIENTS_MODEL_OPENAI")} {
		p := openai.CompletionNewParams{Model: openai.CompletionNewParamsModel(model), Prompt: openai.CompletionNewParamsPromptUnion{OfString: openai.String("Say hello.")}, MaxTokens: openai.Int(32)}
		response, err := c.Completions.New(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if response.Model != model || len(response.Choices) != 1 || response.Choices[0].Text != env(t, "OLP_CLIENTS_DEFAULT_REPLY") || response.Usage.TotalTokens == 0 {
			t.Fatalf("response: %+v", response)
		}
		p.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
		stream := c.Completions.NewStreaming(ctx, p)
		var text strings.Builder
		var tokens int64
		for stream.Next() {
			v := stream.Current()
			for _, choice := range v.Choices {
				text.WriteString(choice.Text)
			}
			if v.Usage.TotalTokens != 0 {
				tokens = v.Usage.TotalTokens
			}
		}
		if err = stream.Err(); err != nil {
			t.Fatal(err)
		}
		stream.Close()
		if text.String() != env(t, "OLP_CLIENTS_DEFAULT_REPLY") || tokens == 0 {
			t.Fatalf("stream: %q tokens=%d", text.String(), tokens)
		}
	}
}
func TestM9PinnedDecisions(t *testing.T) {
	c, ctx := client(t)
	for _, model := range []string{env(t, "OLP_CLIENTS_MODEL_OPENAI_DECISIONS"), env(t, "OLP_CLIENTS_MODEL_SYSTEMONE")} {
		p := openai.DecisionNewParams{Model: model, Input: openai.DecisionNewParamsInputUnion{OfString: openai.String("A refund request.")}, Questions: []openai.DecisionNewParamsQuestionUnion{{OfPredicate: &openai.DecisionNewParamsQuestionPredicate{Name: openai.String("refund"), Instructions: "Is this about a refund?"}}}}
		response, err := c.Decisions.New(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if response.Model != model || len(response.Answers) != 1 || response.Answers[0].Name != "refund" || response.Answers[0].Probability != 0.75 || response.Usage.TotalTokens != 10 && response.Usage.InputTokens+response.Usage.OutputTokens != 10 {
			t.Fatalf("response: %+v", response)
		}
	}
}
func TestM9IncompatibleMappingDoesNotDispatch(t *testing.T) {
	c, ctx := client(t)
	before := requestCount(t, ctx)
	_, err := c.Completions.New(ctx, openai.CompletionNewParams{Model: openai.CompletionNewParamsModel(env(t, "OLP_CLIENTS_MODEL_OPENAI")), Prompt: openai.CompletionNewParamsPromptUnion{OfString: openai.String("hello")}, Echo: openai.Bool(true)})
	if err == nil {
		t.Fatal("echo unexpectedly accepted")
	}
	body := []byte(`{"model":"` + env(t, "OLP_CLIENTS_MODEL_OPENAI_DECISIONS") + `","state":"hello","questions":{"q":{"type":"noul","instructions":"True?"}}}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, env(t, "OLP_CLIENTS_OPENAI_BASE_URL")+"/systemone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env(t, "OLP_CLIENTS_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("mapping status %d: %s", resp.StatusCode, data)
	}
	if after := requestCount(t, ctx); after != before {
		t.Fatalf("incompatible requests dispatched %d upstream attempts", after-before)
	}
}
func TestM9UpstreamCredentialsAndPaths(t *testing.T) {
	_, ctx := client(t)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env(t, "OLP_CLIENTS_UPSTREAM_URL")+"/__recorded", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Requests []struct {
			Path       string `json:"path"`
			Authorized bool   `json:"authorized"`
			Leaked     bool   `json:"leaked_client_credential"`
		}
	}
	if json.NewDecoder(resp.Body).Decode(&data) != nil {
		t.Fatal("recording")
	}
	paths := map[string]bool{}
	for _, r := range data.Requests {
		if !r.Authorized || r.Leaked {
			t.Fatal("credential isolation failed")
		}
		paths[r.Path] = true
	}
	for _, p := range []string{"/openai/v1/completions", "/openai/v1/chat/completions", "/openai/v1/decisions", "/openai/v1/systemone"} {
		if !paths[p] {
			t.Errorf("path not exercised: %s", p)
		}
	}
}

func requestCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env(t, "OLP_CLIENTS_UPSTREAM_URL")+"/__recorded", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct{ Requests []json.RawMessage }
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return len(body.Requests)
}
func TestM9PinnedDecisionRefusal(t *testing.T) {
	c, ctx := client(t)
	response, err := c.Decisions.New(ctx, openai.DecisionNewParams{Model: env(t, "OLP_CLIENTS_MODEL_OPENAI_DECISIONS"), Input: openai.DecisionNewParamsInputUnion{OfString: openai.String("refuse")}, Questions: []openai.DecisionNewParamsQuestionUnion{{OfPredicate: &openai.DecisionNewParamsQuestionPredicate{Name: openai.String("q"), Instructions: "Answer this question."}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Answers) != 1 || response.Answers[0].Type != "refusal" || response.Answers[0].Name != "q" {
		t.Fatalf("refusal not preserved: %+v", response)
	}
}
