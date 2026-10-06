//go:build liveproviders

package estimate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// TestLiveOpenAIPromptTokensMatchTheEstimate sends every text of the token
// corpus to OpenAI in the chat request a client would send it in, the one the
// corpus parity tests read, and expects the prompt tokens OpenAI reports to be
// what the estimate counts: the text exactly, and the framing the cookbook
// documents. A count the estimate does not claim is exact is reported and not
// held to it. It logs the error over the corpus, which the roadmap records.
func TestLiveOpenAIPromptTokensMatchTheEstimate(t *testing.T) {
	if os.Getenv("OLP_LIVE_PROVIDER") != "openai" {
		t.Skip("set OLP_LIVE_PROVIDER=openai; this test incurs provider charges")
	}
	key := os.Getenv("OLP_LIVE_OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OLP_LIVE_OPENAI_API_KEY is required")
	}
	model := cmp.Or(os.Getenv("OLP_LIVE_MODEL"), "gpt-4o-mini")
	endpoint := strings.TrimSuffix(cmp.Or(os.Getenv("OLP_LIVE_OPENAI_BASE_URL"), "https://api.openai.com/v1"), "/") + "/chat/completions"
	counter := ForModel(model)
	if counter.Family() != FamilyOpenAIO200k && counter.Family() != FamilyOpenAICL100k {
		t.Fatalf("%s is in the %s family, not one of OpenAI's", model, counter.Family())
	}
	encoding := "o200k_base.json"
	if counter.Family() == FamilyOpenAICL100k {
		encoding = "cl100k_base.json"
	}

	type outcome struct {
		name               string
		estimate, reported int64
		provenance         Provenance
		err                error
	}
	fixtures := loadFixtures(t, encoding)
	outcomes := make([]outcome, len(fixtures))
	client := &http.Client{Timeout: 60 * time.Second}
	limit := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, f := range fixtures {
		text, _ := json.Marshal(f.Text)
		body := []byte(`{"model":` + string(must(json.Marshal(model))) + `,"max_completion_tokens":1,"messages":[{"role":"system","content":"s"},{"role":"user","content":` + string(text) + `}]}`)
		request, err := openai.Parse(openai.FamilyChat, body)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		count := Walk(request).Input(counter)
		outcomes[i] = outcome{name: f.Name, estimate: count.Tokens, provenance: count.Provenance}
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			outcomes[i].reported, outcomes[i].err = promptTokens(t, client, endpoint, key, body)
		})
	}
	wg.Wait()

	var estimated, reported, exact, compared int64
	for _, o := range outcomes {
		if o.err != nil {
			t.Errorf("%s: %v", o.name, o.err)
			continue
		}
		compared++
		estimated += o.estimate
		reported += o.reported
		switch {
		case o.estimate == o.reported:
			exact++
		case o.provenance == ProvenanceTokenizer:
			t.Errorf("%s: the estimate counts %d prompt tokens, %s reports %d", o.name, o.estimate, model, o.reported)
		default:
			t.Logf("%s: the %s estimate counts %d prompt tokens, %s reports %d", o.name, o.provenance, o.estimate, model, o.reported)
		}
	}
	if compared == 0 {
		t.Fatal("no fixture was compared")
	}
	t.Logf("%s, %s family: %d of %d fixtures counted exactly; %d prompt tokens estimated, %d reported, an error of %+.3f%%",
		model, counter.Family(), exact, compared, estimated, reported, 100*float64(estimated-reported)/float64(reported))
}

// promptTokens sends body and returns the prompt tokens the reply's usage
// reports, retrying a rate limit or a server error a few times.
func promptTokens(t *testing.T, client *http.Client, endpoint, key string, body []byte) (int64, error) {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			last = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("status %d: %.300s", resp.StatusCode, data)
		}
		var reply struct {
			Usage *struct {
				PromptTokens int64 `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &reply); err != nil || reply.Usage == nil {
			return 0, fmt.Errorf("the reply reports no usage: %.300s", data)
		}
		return reply.Usage.PromptTokens, nil
	}
	return 0, last
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
