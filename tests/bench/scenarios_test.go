//go:build bench

// The scenario suite of the gateway benchmark (docs/roadmap/m01-measured-advantage.md).
//
// Each TestScenarioSn starts the mock upstream and a real `olp all` process
// against the disposable PostgreSQL and Valkey that make bench provisions,
// builds an installation for it through the management API, runs a
// direct-to-mock baseline and then the same load through the gateway, and
// writes the result to .local/bench/<scenario>.json. See run_test.go for the
// sequence and README.md for how to read a result.
//
// These tests fail, rather than skip, when the services or binaries they need
// are not configured: a benchmark that silently measures nothing is worse than
// one that does not run. `go test -tags=bench ./tests/bench/...` therefore
// fails here by design; run the scenarios with make bench, and the harness's own
// tests with `go test -tags=bench ./tests/bench/mockupstream ./tests/bench/loadgen`.
package bench_test

import (
	"math"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
	"github.com/tyk-swe/olp/tests/bench/mockupstream"
)

// plan is a scenario's workload and topology once the settings have scaled it.
type plan struct {
	// Dialect is the wire format the client speaks and SurfacePath where the
	// gateway serves it: "" for OpenAI Chat Completions, /anthropic for
	// Messages.
	Dialect     loadgen.Dialect `json:"dialect"`
	SurfacePath string          `json:"surface_path,omitempty"`
	Rate        float64         `json:"rate_rps"`
	StreamShare float64         `json:"stream_share"`
	// PromptTokens lists the prompt sizes used in equal shares; 0 is a short
	// prompt.
	PromptTokens []int `json:"prompt_tokens"`
	MaxTokens    int   `json:"max_tokens"`

	// Mock is the upstream's behavior for every model. Models are the upstream
	// models a route targets, in the order of its targets, and Baseline the
	// one a direct run asks for: the one that succeeds.
	Mock     mockupstream.Rule `json:"mock_behavior"`
	Models   []string          `json:"upstream_models"`
	Baseline string            `json:"baseline_model"`

	DurationSeconds float64 `json:"duration_seconds"`
	WarmupSeconds   float64 `json:"warmup_seconds"`
	TimeoutSeconds  float64 `json:"timeout_seconds"`
	// DrainSeconds is how long the generator waits for requests still open
	// when its schedule ends; 0 leaves the generator's default. S6 uses it as
	// the time the full set of streams is held.
	DrainSeconds float64 `json:"drain_seconds,omitempty"`
	// Concurrency is the most requests expected in flight at once: the rate
	// times the time an upstream holds one. The gateway's limits and the
	// generator's bounds are sized from it.
	Concurrency int `json:"expected_concurrency"`

	// Surfaces are the surfaces the upstream model is declared for, when the
	// clients speak a dialect the provider does not.
	Surfaces []string `json:"model_surfaces,omitempty"`
	// Failover routes through two providers, the first of which fails.
	Failover bool `json:"failover,omitempty"`
	// Budget gives the key a cost budget and prices the model.
	Budget bool `json:"cost_budget,omitempty"`
	// Shadow adds a shadow target, of shadowModel, that mirrors every request.
	Shadow bool `json:"shadow,omitempty"`
	// SlowReadBPS makes readers consume each stream at this many bytes per
	// second.
	SlowReadBPS int `json:"slow_read_bytes_per_second,omitempty"`
	// StreamBytes is the size of one stream measured against the mock.
	StreamBytes int64 `json:"stream_bytes,omitempty"`
}

// scenario is one row of the roadmap's scenario table.
type scenario struct {
	ID    string
	Title string
	// Why is the table's reason for the scenario.
	Why  string
	plan func(settings) plan
	// tune finishes a plan that depends on the running mock.
	tune func(*testing.T, settings, *mockProcess, *plan)
}

const (
	routeSlug = "bench-route"

	// The upstream's time to first token in every scenario but S6. Gateway
	// overhead is a difference against a baseline that has the same upstream,
	// so its value matters only in setting how many requests are in flight.
	upstreamTTFT = 20.0
	// A cost budget no scenario can exhaust, so that it is consulted and
	// accrued against on every request without ever rejecting one.
	benchBudget = "1000000"
	// The prompt sizes of S3, in equal shares: LiteLLM's high-throughput
	// benchmark.
	tokens50k, tokens75k, tokens100k = 50_000, 75_000, 100_000
	// The upstream model of S3. OpenAI's naming puts it in the o200k family, so
	// admission counts a prompt with the exact encoder. The mock matches it by
	// name, and deploy/litellm calls the same one.
	s3Model = "gpt-4o-bench"
	// shadowModel is the upstream model of S1-shadow's shadow target.
	shadowModel = "bench-shadow"
)

// rule is a mock behavior with the given timing and completion length.
func rule(ttft, interval float64, tokens int) mockupstream.Rule {
	return mockupstream.Rule{TTFTMs: &ttft, IntervalMs: &interval, OutputTokens: &tokens}
}

// holdSeconds is how long the mock holds a request of that behavior: the time
// of its last frame, which is also how long a unary response is held.
func holdSeconds(ttft, interval float64, tokens int) float64 {
	return (ttft + float64(tokens-1)*interval) / 1000
}

// concurrencyFor is the in-flight requests a rate implies, rounded up.
func concurrencyFor(rate, hold float64) int { return max(1, int(math.Ceil(rate*hold))) }

// base is the plan every scenario but S6 starts from.
func base(s settings, rate float64, behavior mockupstream.Rule, tokens int) plan {
	rate = math.Max(1, math.Round(rate*s.Scale))
	hold := holdSeconds(upstreamTTFT, *behavior.IntervalMs, tokens)
	return plan{
		Dialect: loadgen.OpenAI, Rate: rate, PromptTokens: []int{0}, MaxTokens: tokens,
		Mock: behavior, Models: []string{"bench-chat"}, Baseline: "bench-chat",
		DurationSeconds: s.Duration.Seconds(), WarmupSeconds: s.Warmup.Seconds(), TimeoutSeconds: 30,
		Concurrency: concurrencyFor(rate, hold),
	}
}

var (
	// S1: LiteLLM's headline scenario, 8 ms P95 at 1,000 RPS.
	s1 = scenario{
		ID: "S1", Title: "Chat Completions, short prompt, unary, 1,000 RPS",
		Why: "LiteLLM's headline 8 ms P95 scenario",
		plan: func(s settings) plan {
			return base(s, 1000, rule(upstreamTTFT, 0, 16), 16)
		},
	}
	// S2: time to first token and the cost of relaying a stream.
	s2 = scenario{
		ID: "S2", Title: "Chat Completions, short prompt, streaming 64 tokens at 20 ms intervals, 1,000 RPS",
		Why: "Time-to-first-token overhead and stream relay cost",
		plan: func(s settings) plan {
			p := base(s, 1000, rule(upstreamTTFT, 20, 64), 64)
			p.StreamShare = 1
			return p
		},
	}
	// S3: LiteLLM's high-throughput benchmark. The prompts are what makes it
	// heavy: 50K to 100K tokens is 260 to 520 KB a request, tokenized for
	// admission and reserved against a budget. They are tokenized only for a
	// model whose tokenizer the gateway has, so S3's upstream model is named as
	// an OpenAI model is: the other scenarios' models are of the family the
	// gateway cannot name, and are charged four characters to a token, which is
	// the exact encoder's work left out of the measurement.
	s3 = scenario{
		ID: "S3", Title: "50K, 75K and 100K-token prompts in equal shares, 50% streaming, max_tokens 16, key with a cost budget, 3,000 RPS",
		Why: "LiteLLM's high-throughput benchmark, including admission token estimation and budget reservation",
		plan: func(s settings) plan {
			p := base(s, 3000, rule(upstreamTTFT, 2, 16), 16)
			p.StreamShare = 0.5
			p.PromptTokens = []int{tokens50k, tokens75k, tokens100k}
			p.Models, p.Baseline = []string{s3Model}, s3Model
			p.Budget = true
			return p
		},
	}
	// S4: the first target answers 503 and the second succeeds.
	s4 = scenario{
		ID: "S4", Title: "First target returns 503, second succeeds",
		Why: "Failover cost",
		plan: func(s settings) plan {
			p := base(s, 1000, rule(upstreamTTFT, 0, 16), 16)
			p.Models, p.Baseline = []string{"bench-primary", "bench-secondary"}, "bench-secondary"
			p.Failover = true
			return p
		},
	}
	// S5: a client of one dialect on a route that speaks another.
	s5 = scenario{
		ID: "S5", Title: "Anthropic Messages streaming translated to an OpenAI upstream",
		Why: "Translation cost on transformed routes",
		plan: func(s settings) plan {
			p := base(s, 1000, rule(upstreamTTFT, 20, 64), 64)
			p.Dialect, p.SurfacePath, p.StreamShare = loadgen.Anthropic, "/anthropic", 1
			p.Surfaces = []string{"openai", "anthropic"}
			return p
		},
	}
	// S1-shadow: S1 with every request mirrored to a shadow target, which
	// must leave the caller's latency where S1 has it.
	s1Shadow = scenario{
		ID: "S1-shadow", Title: "S1 with every request mirrored to a shadow target",
		Why: "Shadow traffic must not change caller-visible latency",
		plan: func(s settings) plan {
			p := s1.plan(s)
			p.Shadow = true
			return p
		},
	}
	// S6: ten thousand streams held open by readers that barely read.
	s6 = scenario{
		ID: "S6", Title: "10,000 concurrent streams with slow readers",
		Why:  "Memory, goroutines and write-deadline behavior at scale",
		plan: s6Plan,
		tune: s6Tune,
	}
)

const s6Streams = 10_000

// S6 opens its streams over at most s6MaxWarmup and s6MaxDuration, however
// long the other scenarios run. The gateway ends a stream thirty seconds after
// its writes block, which at the readers' pace is soon after it opens, so over
// the default seventy seconds the first streams are gone before the last open.
const (
	s6MaxWarmup   = 3 * time.Second
	s6MaxDuration = 15 * time.Second
)

// s6Plan is the shape of the slow-reader scenario. The upstream writes a long
// completion back to back, and each reader takes it at a few kilobytes a
// second, so no stream finishes while the run lasts: the generator opens them
// at a constant rate over its schedule, reaches the full count when the
// schedule ends, and then holds them for the drain period before it cancels
// what is left. The completion has to be longer than the socket buffers along
// the path, or the gateway's writes would complete into them and never meet
// the slow reader: on loopback the gateway's send buffer alone is about 2.6 MB.
func s6Plan(s settings) plan {
	streams := scaled(s6Streams, s.Scale)
	tokens := s.S6Tokens
	warmup, duration := min(s.Warmup, s6MaxWarmup), min(s.Duration, s6MaxDuration)
	p := plan{
		Dialect: loadgen.OpenAI, StreamShare: 1, PromptTokens: []int{0}, MaxTokens: tokens,
		Mock: rule(upstreamTTFT, 0, tokens), Models: []string{"bench-chat"}, Baseline: "bench-chat",
		Concurrency: streams, SlowReadBPS: s.S6BPS,
		WarmupSeconds: warmup.Seconds(), DurationSeconds: duration.Seconds(), DrainSeconds: duration.Seconds(),
	}
	p.Rate = math.Max(0.1, float64(streams)/(p.WarmupSeconds+p.DurationSeconds))
	p.TimeoutSeconds = p.WarmupSeconds + p.DurationSeconds + p.DrainSeconds + 60
	return p
}

// s6Tune checks, against the running mock, that no stream can finish within
// the run: a reader at the plan's pace takes SlowReadBPS * the run's length
// bytes at most.
func s6Tune(t *testing.T, s settings, m *mockProcess, p *plan) {
	t.Helper()
	p.StreamBytes = measureStream(t, m, p.Baseline)
	run := p.WarmupSeconds + p.DurationSeconds + p.DrainSeconds
	if need := int64(float64(p.SlowReadBPS) * run * 1.1); p.StreamBytes < need {
		t.Fatalf("a stream of %d bytes would finish within the %.0f seconds of the run at %d bytes a second, and S6 holds streams open: "+
			"raise %s above %d tokens, lower %s or shorten %s", p.StreamBytes, run, p.SlowReadBPS, envS6Tokens, int(float64(s.S6Tokens)*float64(need)/float64(p.StreamBytes))+1, envS6BPS, envDuration)
	}
}

func (p plan) duration() time.Duration { return seconds(p.DurationSeconds) }
func (p plan) warmup() time.Duration   { return seconds(p.WarmupSeconds) }
func (p plan) timeout() time.Duration  { return seconds(p.TimeoutSeconds) }
func (p plan) drain() time.Duration    { return seconds(p.DrainSeconds) }

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// provisioningSpec is the mock's configuration while the installation is
// provisioned: every model of the plan answers briefly and immediately.
func (p plan) provisioningSpec() mockupstream.Spec {
	spec := mockupstream.Spec{Default: rule(0, 0, 16), Models: map[string]mockupstream.Rule{}}
	for _, model := range p.Models {
		spec.Models[model] = mockupstream.Rule{}
	}
	return spec
}

// spec is the mock's configuration: its default behavior and one rule per
// upstream model, which inherit it. With failing set, the first of a failover
// route's models answers 503 to everything.
func (p plan) spec(failing bool) mockupstream.Spec {
	spec := mockupstream.Spec{Default: p.Mock, Models: map[string]mockupstream.Rule{}}
	for _, model := range p.Models {
		spec.Models[model] = mockupstream.Rule{}
	}
	if failing {
		status := 503
		spec.Models[p.failingModel()] = mockupstream.Rule{Status: &status}
	}
	return spec
}

// failingModel is the model of a failover route's first target.
func (p plan) failingModel() string { return p.Models[0] }

// loadConfig is the generator's configuration for a target. A baseline run
// names the upstream model and a gateway run the route.
func (p plan) loadConfig(s settings, target, model, key string) loadgen.Config {
	return loadgen.Config{
		URL: target, Dialect: p.Dialect, APIKey: key, Model: model,
		Rate: p.Rate, Duration: p.duration(), Warmup: p.warmup(),
		StreamShare: p.StreamShare, PromptTokens: p.PromptTokens, MaxTokens: p.MaxTokens,
		// Room for every request the schedule can have open, and as much again
		// waiting, so that queueing shows in the latency and nothing is dropped.
		MaxInFlight: p.Concurrency*2 + 1024,
		Timeout:     p.timeout(), Drain: p.drain(), LateAfter: s.LateAfter,
		SlowRead: loadgen.SlowRead{BytesPerSecond: p.SlowReadBPS},
	}
}

func TestScenarioS1(t *testing.T)       { runScenario(t, s1) }
func TestScenarioS1Shadow(t *testing.T) { runScenario(t, s1Shadow) }
func TestScenarioS2(t *testing.T)       { runScenario(t, s2) }
func TestScenarioS3(t *testing.T)       { runScenario(t, s3) }
func TestScenarioS4(t *testing.T)       { runScenario(t, s4) }
func TestScenarioS5(t *testing.T)       { runScenario(t, s5) }
func TestScenarioS6(t *testing.T)       { runScenario(t, s6) }
