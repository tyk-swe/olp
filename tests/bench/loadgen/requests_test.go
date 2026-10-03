//go:build bench

package loadgen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// captured is a request as the server saw it.
type captured struct {
	method, path, rawQuery string
	header                 http.Header
	body                   []byte
	contentLength          int64
}

// capture serves 200 and keeps every request.
func capture() (http.Handler, func() []captured) {
	var mu sync.Mutex
	var got []captured
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body, r.ContentLength})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	})
	return h, func() []captured { mu.Lock(); defer mu.Unlock(); return append([]captured(nil), got...) }
}

func TestRequestShapePerDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect     Dialect
		path, query string
		authHeader  string
		authValue   string
		streamPath  string
		streamQuery string
	}{
		{OpenAI, "/v1/chat/completions", "", "Authorization", "Bearer secret", "/v1/chat/completions", ""},
		{Anthropic, "/v1/messages", "", "X-Api-Key", "secret", "/v1/messages", ""},
		{Gemini, "/v1beta/models/the-model:generateContent", "", "X-Goog-Api-Key", "secret", "/v1beta/models/the-model:streamGenerateContent", "alt=sse"},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, got := capture()
				cfg := base(h)
				cfg.Dialect, cfg.APIKey, cfg.Model = tc.dialect, "secret", "the-model"
				cfg.Rate, cfg.Duration, cfg.StreamShare, cfg.PromptTokens, cfg.MaxTokens = 40, time.Second, 0.5, []int{0, 300}, 16
				cfg.Headers = map[string]string{"X-Extra": "1"}
				if report := run(t, cfg); report.Requests.Sent != 40 {
					t.Fatalf("%+v", report.Requests)
				}
				requests := got()
				if len(requests) != 40 {
					t.Fatalf("%d requests", len(requests))
				}
				streams, long, ids := 0, 0, map[string]bool{}
				for _, r := range requests {
					var doc map[string]json.RawMessage
					if err := json.Unmarshal(r.body, &doc); err != nil {
						t.Fatalf("the body is not JSON: %v\n%.200s", err, r.body)
					}
					if int64(len(r.body)) != r.contentLength {
						t.Fatalf("content length %d, body %d bytes", r.contentLength, len(r.body))
					}
					streaming := false
					switch tc.dialect {
					case OpenAI, Anthropic:
						streaming = string(doc["stream"]) == "true"
						if string(doc["model"]) != `"the-model"` || string(doc["max_tokens"]) != "16" {
							t.Fatalf("body %.200s", r.body)
						}
					case Gemini:
						streaming = strings.Contains(r.path, "streamGenerateContent")
						if !strings.Contains(string(doc["generationConfig"]), `"maxOutputTokens":16`) {
							t.Fatalf("body %.200s", r.body)
						}
					}
					wantPath, wantQuery := tc.path, tc.query
					if streaming {
						wantPath, wantQuery = tc.streamPath, tc.streamQuery
					}
					if r.method != http.MethodPost || r.path != wantPath || r.rawQuery != wantQuery {
						t.Fatalf("%s %s?%s, want POST %s?%s", r.method, r.path, r.rawQuery, wantPath, wantQuery)
					}
					if r.header.Get(tc.authHeader) != tc.authValue || r.header.Get("Content-Type") != "application/json" || r.header.Get("X-Extra") != "1" {
						t.Fatalf("headers %v", r.header)
					}
					if tc.dialect == Anthropic && r.header.Get("Anthropic-Version") == "" {
						t.Fatal("Anthropic requires anthropic-version")
					}
					if streaming {
						streams++
						if tc.dialect == OpenAI && !strings.Contains(string(r.body), `"stream_options":{"include_usage":true}`) {
							t.Fatalf("a streaming OpenAI request asks for usage: %.300s", r.body)
						}
					}
					if len(r.body) > 1000 {
						long++
					}
					// Each request is named, so none repeats another byte for byte.
					id := string(r.body[:min(len(r.body), 300)])
					ids[id[strings.Index(id, "request"):strings.Index(id, ": ")]] = true
				}
				if streams != 20 || long != 20 || len(ids) != 40 {
					t.Fatalf("%d streaming, %d long, %d distinct of 40", streams, long, len(ids))
				}
			})
		})
	}
}

func TestBodiesAreWellFormedForEverySizeAndMode(t *testing.T) {
	for _, d := range []Dialect{OpenAI, Anthropic, Gemini} {
		for _, tokens := range []int{0, 1, 17, 5000} {
			cfg, err := Config{URL: "http://x", Dialect: d, Model: `we"ird\model`, Rate: 1, Duration: time.Second, PromptTokens: []int{tokens}, StreamShare: 1}.normalize()
			if err != nil {
				t.Fatal(err)
			}
			b, err := newBodies(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for k := int64(0); k < 4; k++ {
				req, n, err := b.request(k)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(req.Body)
				var doc map[string]any
				if err := json.Unmarshal(body, &doc); err != nil || int64(len(body)) != n || req.ContentLength != n {
					t.Fatalf("%s %d tokens: %v, %d bytes, content length %d, counted %d", d, tokens, err, len(body), req.ContentLength, n)
				}
				// The same request can be built again, as a redirect or retry needs.
				again, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				if body2, _ := io.ReadAll(again); string(body2) != string(body) {
					t.Fatal("GetBody returned a different body")
				}
			}
		}
	}
}

func TestPromptMixAndStreamShareAreEvenlySpread(t *testing.T) {
	cfg, err := Config{URL: "http://x", Model: "m", Rate: 1, Duration: time.Second, PromptTokens: []int{50_000, 75_000, 100_000}, StreamShare: 0.5}.normalize()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newBodies(cfg)
	sizes := map[int]int{}
	streams := 0
	for k := int64(0); k < 6000; k++ {
		sizes[b.size(k)]++
		if b.streams(k) {
			streams++
		}
	}
	if sizes[0] != 2000 || sizes[1] != 2000 || sizes[2] != 2000 || streams != 3000 {
		t.Fatalf("sizes %v, %d streams", sizes, streams)
	}
	// Every window of requests has the same mix, so a short run is not skewed.
	for start := int64(0); start < 120; start++ {
		n := 0
		for k := start; k < start+6; k++ {
			if b.streams(k) {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("%d of the 6 requests from %d stream", n, start)
		}
	}
	for _, share := range []float64{0, 0.1, 0.3, 0.9, 1} {
		cfg.StreamShare = share
		b, _ := newBodies(cfg)
		n := 0
		for k := int64(0); k < 1000; k++ {
			if b.streams(k) {
				n++
			}
		}
		if n != int(share*1000+0.5) {
			t.Errorf("share %v produced %d of 1000", share, n)
		}
	}
	// The sizes are in tokens: 100K tokens of the corpus weigh about 520 KB, as
	// the README says, which is about 5.2 bytes to a word.
	for i, tokens := range []int{50_000, 75_000, 100_000} {
		if got := len(b.prompts[i]); got < tokens*51/10 || got > tokens*53/10 {
			t.Errorf("%d tokens weigh %d bytes, not the 5.1 to 5.3 bytes a token the documentation gives", tokens, got)
		}
	}
}

// TestPromptSizesAreIndependentOfTheMode guards against a confound: with an
// even number of sizes and a stream share of one half, rotating the sizes over
// the request number would give every streaming request one size and every
// unary request the other, and the two modes' latencies would then differ by
// prompt as much as by mode.
func TestPromptSizesAreIndependentOfTheMode(t *testing.T) {
	for _, tc := range []struct {
		sizes []int
		share float64
	}{
		{[]int{0, 2000}, 0.5},
		{[]int{0, 2000}, 0.25},
		{[]int{0, 1000, 2000, 3000}, 0.5},
		{[]int{50_000, 75_000, 100_000}, 0.5},
		{[]int{50_000, 75_000, 100_000}, 0.3},
		{[]int{0, 2000, 4000}, 1},
	} {
		cfg, err := Config{URL: "http://x", Model: "m", Rate: 1, Duration: time.Second, PromptTokens: tc.sizes, StreamShare: tc.share}.normalize()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := newBodies(cfg)
		// Requests of each mode that used each size.
		counts := [2][]int{make([]int, len(tc.sizes)), make([]int, len(tc.sizes))}
		total := [2]int{}
		for k := int64(0); k < 12_000; k++ {
			mode := 0
			if b.streams(k) {
				mode = 1
			}
			counts[mode][b.size(k)]++
			total[mode]++
		}
		for mode, name := range []string{"unary", "stream"} {
			if total[mode] == 0 {
				continue
			}
			lo, hi := slices.Min(counts[mode]), slices.Max(counts[mode])
			if hi-lo > 1 {
				t.Errorf("sizes %v, share %v: %s requests used the sizes %v times, which is not an equal share", tc.sizes, tc.share, name, counts[mode])
			}
		}
	}
}

func TestPromptsAreDeterministicAndJSONSafe(t *testing.T) {
	a, b := prompt(1000), prompt(1000)
	if string(a) != string(b) {
		t.Fatal("two prompts of the same size differ")
	}
	if got := len(strings.Fields(string(a))); got != 1000 {
		t.Fatalf("%d words, want 1000", got)
	}
	// The prompt is a prefix of the longer one, so sizes are comparable.
	if !strings.HasPrefix(string(prompt(2000)), string(a)) {
		t.Fatal("a longer prompt does not extend a shorter one")
	}
	sum := sha256.Sum256(a)
	if got := hex.EncodeToString(sum[:]); got != promptDigest {
		t.Fatalf("the corpus or its sequence changed: %s\nrepublishing results across a change makes them incomparable; update promptDigest deliberately", got)
	}
	for _, w := range vocabulary {
		if w == "" || strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Errorf("word %q is not plain lowercase letters", w)
		}
	}
	seen := map[string]bool{}
	for _, w := range vocabulary {
		if seen[w] {
			t.Errorf("duplicate word %q", w)
		}
		seen[w] = true
	}
	if len(vocabulary) < 200 {
		t.Errorf("%d words is too few for a prompt that does not repeat visibly", len(vocabulary))
	}
	if string(prompt(0)) != shortPrompt || string(prompt(-3)) != shortPrompt {
		t.Error("the short prompt")
	}
	// Every word is used, and in no obvious order.
	used := map[string]bool{}
	for _, w := range strings.Fields(string(prompt(20_000))) {
		used[w] = true
	}
	if len(used) != len(vocabulary) {
		t.Errorf("only %d of %d words appear", len(used), len(vocabulary))
	}
}

// promptDigest pins prompt(1000).
const promptDigest = "688b6f145683f15b55e4f01fb95ba036c437b43a6b28abe399e6ec6befec3eb6"

func TestEndpointURLs(t *testing.T) {
	for _, tc := range []struct {
		base, path, model string
		dialect           Dialect
		unary, stream     string
	}{
		{"http://h:1", "", "m", OpenAI, "http://h:1/v1/chat/completions", "http://h:1/v1/chat/completions"},
		{"http://h:1/", "", "m", OpenAI, "http://h:1/v1/chat/completions", "http://h:1/v1/chat/completions"},
		{"https://h/prefix/", "", "m", Anthropic, "https://h/prefix/v1/messages", "https://h/prefix/v1/messages"},
		{"http://h/litellm", "", "gemini-x", Gemini, "http://h/litellm/v1beta/models/gemini-x:generateContent", "http://h/litellm/v1beta/models/gemini-x:streamGenerateContent?alt=sse"},
		{"http://h", "/chat/completions", "m", OpenAI, "http://h/chat/completions", "http://h/chat/completions"},
		{"http://h", "v1/custom/{model}/{operation}", "a b", Gemini, "http://h/v1/custom/a%20b/generateContent", "http://h/v1/custom/a%20b/streamGenerateContent?alt=sse"},
	} {
		cfg, err := Config{URL: tc.base, Path: tc.path, Dialect: tc.dialect, Model: tc.model, Rate: 1, Duration: time.Second}.normalize()
		if err != nil {
			t.Fatal(err)
		}
		b, err := newBodies(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.urls[0].String(); got != tc.unary {
			t.Errorf("%+v unary %s, want %s", tc, got, tc.unary)
		}
		if got := b.urls[1].String(); got != tc.stream {
			t.Errorf("%+v stream %s, want %s", tc, got, tc.stream)
		}
	}
}
