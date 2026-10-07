package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Conditions that start a fallback to another route.
const (
	// FallbackExhausted means every attempt the route made failed with a class
	// that may move to another target, and none remained.
	FallbackExhausted     = "exhausted"
	FallbackContextWindow = "context_window"
	FallbackContentFilter = "content_filter"
	FallbackRateLimit     = "rate_limit"
	// FallbackBudget means a supply-side cost cap removed the route or every
	// target it could have used.
	FallbackBudget = "budget"
)

// FallbackConditions lists every condition a fallback may name, in the order
// documentation and validation present them.
var FallbackConditions = []string{FallbackExhausted, FallbackContextWindow, FallbackContentFilter, FallbackRateLimit, FallbackBudget}

// RetryClasses are the failure classes a route may retry on the same target.
// Each is retryable before commitment; a credential or context-window failure
// would only fail again.
var RetryClasses = []string{"connect", "timeout", "rate_limit", "upstream_server"}

// Session affinity sources.
const (
	AffinityLabel    = "label"
	AffinityCacheKey = "cache_key"
)

// Limits on a route's adaptive behavior. MaxRouteDepth counts routes, so a
// primary may fall back to a route that falls back once more.
const (
	MaxRouteDepth    = 3
	maxFallbacks     = 4
	maxSelectors     = 16
	maxSelectorTags  = 8
	maxTargetTags    = 8
	maxRetries       = 10
	maxBackoffMS     = 60000
	maxClassifierMS  = 30000
	maxPredicateList = 16
)

var (
	pluginDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	selectorID   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	targetTag    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	effortName   = regexp.MustCompile(`^[a-z]{1,16}$`)
	// Modalities are the kinds of input a request can carry.
	Modalities = []string{"text", "image", "audio", "video", "file"}
)

// Behavior is the adaptive routing a route revision declares beyond its
// targets: where a failed request goes next, which targets a request is
// steered to, how a failed attempt is retried, which requests stay together
// and how much the route may spend. The zero value adds nothing to a route.
type Behavior struct {
	Fallbacks []Fallback  `json:"fallbacks,omitempty"`
	Selectors []Selector  `json:"selectors,omitempty"`
	Retry     Retry       `json:"retry,omitempty"`
	Affinity  *Affinity   `json:"affinity,omitempty"`
	Budget    *CostLimits `json:"budget,omitempty"`
}

// Fallback names another route that serves the request when this one ends
// with one of the On conditions.
type Fallback struct {
	Route string   `json:"route"`
	On    []string `json:"on"`
}

// Met returns the conditions of a failed route that start this fallback.
func (f Fallback) Met(conditions []string) []string {
	var met []string
	for _, condition := range f.On {
		if slices.Contains(conditions, condition) {
			met = append(met, condition)
		}
	}
	return met
}

// FallbackStep explains one declared fallback a request reached: the route
// that declared it, the conditions that started it, and whether the fallback
// route planned attempts or why it was skipped.
type FallbackStep struct {
	From       string   `json:"from"`
	Route      string   `json:"route"`
	Conditions []string `json:"conditions"`
	Outcome    string   `json:"outcome"`
}

// Fallback step outcomes other than the planning refusal of the route.
const (
	FallbackPlanned     = "planned"
	FallbackUnavailable = "fallback_route_unavailable"
	FallbackForbidden   = "fallback_route_forbidden"
	FallbackRepeated    = "fallback_route_repeated"
)

// Selector steers requests whose features satisfy When to the targets that
// carry one of Tags, or delegates them to Route.
type Selector struct {
	ID    string    `json:"id"`
	When  Predicate `json:"when"`
	Tags  []string  `json:"tags,omitempty"`
	Route string    `json:"route,omitempty"`
}

// Predicate is a conjunction of tests over the features the gateway computes
// while admitting a request. An empty predicate matches every request.
type Predicate struct {
	Operations       []string             `json:"operations,omitempty"`
	MinInputTokens   *int64               `json:"min_input_tokens,omitempty"`
	MaxInputTokens   *int64               `json:"max_input_tokens,omitempty"`
	MinOutputTokens  *int64               `json:"min_output_tokens,omitempty"`
	MaxOutputTokens  *int64               `json:"max_output_tokens,omitempty"`
	Streaming        *bool                `json:"streaming,omitempty"`
	Tools            *bool                `json:"tools,omitempty"`
	Modalities       []string             `json:"modalities,omitempty"`
	StructuredOutput *bool                `json:"structured_output,omitempty"`
	ReasoningEffort  []string             `json:"reasoning_effort,omitempty"`
	Classifier       *ClassifierPredicate `json:"classifier,omitempty"`
	Plugin           *PluginPredicate     `json:"plugin,omitempty"`
}

// ClassifierPredicate sends the request text to another route and matches
// when the label it returns is one of Labels.
type ClassifierPredicate struct {
	Route     string   `json:"route"`
	Labels    []string `json:"labels"`
	MinScore  *float64 `json:"min_score,omitempty"`
	TimeoutMS int64    `json:"timeout_ms"`
}

// Accepts reports whether a classifier's answer satisfies the predicate: one of
// its labels, scored at least its minimum when it names one. An answer without
// a score, such as a generated label, meets any minimum.
func (c *ClassifierPredicate) Accepts(label string, score *float64) bool {
	if !slices.Contains(c.Labels, label) {
		return false
	}
	return c.MinScore == nil || score == nil || *score >= *c.MinScore
}

// PluginPredicate asks the confined route_predicate hook of the approved
// plugin with Digest.
type PluginPredicate struct {
	Digest string `json:"digest"`
}

// Retry maps a retryable failure class to how often and how patiently the
// same target is tried again.
type Retry map[string]RetryRule

// RetryRule is the same-target retry policy for one failure class.
type RetryRule struct {
	MaxRetries        int   `json:"max_retries"`
	BaseBackoffMS     int64 `json:"base_backoff_ms"`
	MaxBackoffMS      int64 `json:"max_backoff_ms"`
	RespectRetryAfter bool  `json:"respect_retry_after"`
}

// Affinity keeps requests that carry the same session key on one target.
type Affinity struct {
	Source string `json:"source"`
	Label  string `json:"label,omitempty"`
}

// SessionKey is the key that keeps a request with its session: the configured
// attribution label, or the dialect's own prompt cache key. It is empty when
// the request carries none.
func (a *Affinity) SessionKey(labels map[string]string, request *openai.Request) string {
	if a == nil {
		return ""
	}
	var key string
	switch a.Source {
	case AffinityLabel:
		key = labels[a.Label]
	case AffinityCacheKey:
		if request != nil {
			_ = json.Unmarshal(request.Field("prompt_cache_key"), &key)
		}
	}
	return key
}

// Seed is the rendezvous seed of a request on the route: its session key when
// it carries one, so every request of the session ranks targets and slots
// alike, and otherwise the caller's own seed.
func (a *Affinity) Seed(labels map[string]string, request *openai.Request, seed []byte) []byte {
	key := a.SessionKey(labels, request)
	if key == "" {
		return seed
	}
	return append([]byte("session\x00"), key...)
}

// CostLimits are daily and monthly spend caps in the installation currency.
type CostLimits struct {
	DailyCostLimit   *string `json:"daily_cost_limit,omitempty"`
	MonthlyCostLimit *string `json:"monthly_cost_limit,omitempty"`
}

// Shadow declares a target that receives a sampled mirror of the route's
// traffic and never answers the caller.
type Shadow struct {
	SampleRate float64 `json:"sample_rate"`
}

// Features are what a selector predicate can observe about a request.
// Attribution labels are deliberately absent.
type Features struct {
	Operation        string   `json:"operation"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens,omitempty"`
	Streaming        bool     `json:"streaming"`
	Tools            bool     `json:"tools"`
	Modalities       []string `json:"modalities"`
	StructuredOutput bool     `json:"structured_output"`
	ReasoningEffort  string   `json:"reasoning_effort,omitempty"`
}

// Refusal is a route publication the author must correct. Publication
// reports it as a 422 with its code.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// DecodeBehavior reads stored or submitted behavior. Absent, null and empty
// documents are the zero behavior; unknown fields are refused.
func DecodeBehavior(raw []byte) (Behavior, error) {
	var b Behavior
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return b, nil
	}
	d := json.NewDecoder(bytes.NewReader(trimmed))
	d.DisallowUnknownFields()
	if err := d.Decode(&b); err != nil || d.Decode(new(any)) != io.EOF {
		return Behavior{}, errors.New("route behavior is malformed")
	}
	return b, nil
}

// IsZero reports whether the behavior declares nothing.
func (b Behavior) IsZero() bool {
	return len(b.Fallbacks) == 0 && len(b.Selectors) == 0 && len(b.Retry) == 0 && b.Affinity == nil && b.Budget == nil
}

// Validate checks the behavior's own shape. tagged reports whether a primary
// target of the route carries a tag, so a selector cannot steer to nothing.
func (b Behavior) Validate(slug string, tagged func(string) bool) error {
	if len(b.Fallbacks) > maxFallbacks {
		return access.Invalid("fallbacks", "A route declares at most "+strconv.Itoa(maxFallbacks)+" fallbacks")
	}
	seen := map[string]bool{}
	for _, f := range b.Fallbacks {
		if !RouteSlug.MatchString(f.Route) || f.Route == slug {
			return access.Invalid("fallbacks", "Each fallback names another route by slug")
		}
		if seen[f.Route] {
			return access.Invalid("fallbacks", "Each fallback route appears once")
		}
		seen[f.Route] = true
		if len(f.On) == 0 || !distinctMembers(f.On, FallbackConditions) {
			return access.Invalid("fallbacks", "Each fallback names distinct conditions among exhausted, context_window, content_filter, rate_limit and budget")
		}
	}
	if err := b.validateSelectors(slug, tagged); err != nil {
		return err
	}
	for class, rule := range b.Retry {
		if !slices.Contains(RetryClasses, class) {
			return access.Invalid("retry", "Retries apply to connect, timeout, rate_limit and upstream_server failures")
		}
		if rule.MaxRetries < 1 || rule.MaxRetries > maxRetries || rule.BaseBackoffMS < 1 || rule.MaxBackoffMS < rule.BaseBackoffMS || rule.MaxBackoffMS > maxBackoffMS {
			return access.Invalid("retry", "Each retry rule allows 1 to 10 retries with a base backoff no larger than its maximum of at most 60000 ms")
		}
	}
	if a := b.Affinity; a != nil {
		switch {
		case a.Source == AffinityLabel && attributionLabel.MatchString(a.Label):
		case a.Source == AffinityCacheKey && a.Label == "":
		default:
			return access.Invalid("affinity", "Affinity reads an attribution label or the dialect's cache key")
		}
	}
	if b.Budget != nil {
		if err := b.Budget.Validate("budget"); err != nil {
			return err
		}
	}
	return nil
}

var attributionLabel = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)

func (b Behavior) validateSelectors(slug string, tagged func(string) bool) error {
	if len(b.Selectors) > maxSelectors {
		return access.Invalid("selectors", "A route declares at most "+strconv.Itoa(maxSelectors)+" selectors")
	}
	ids := map[string]bool{}
	for _, s := range b.Selectors {
		invalid := func(message string) error { return access.Invalid("selectors", "Selector `"+s.ID+"`: "+message) }
		if !selectorID.MatchString(s.ID) || ids[s.ID] {
			return access.Invalid("selectors", "Each selector has a distinct lowercase identifier")
		}
		ids[s.ID] = true
		switch {
		case len(s.Tags) > 0 && s.Route == "":
			if len(s.Tags) > maxSelectorTags || !distinct(s.Tags) {
				return invalid("name at most 8 distinct target tags")
			}
			for _, tag := range s.Tags {
				if !targetTag.MatchString(tag) || tagged != nil && !tagged(tag) {
					return invalid("tag `" + tag + "` is carried by no target")
				}
			}
		case len(s.Tags) == 0 && s.Route != "":
			if !RouteSlug.MatchString(s.Route) || s.Route == slug {
				return invalid("delegate to another route by slug")
			}
		default:
			return invalid("choose target tags or delegate to a route")
		}
		if err := s.When.validate(slug, invalid); err != nil {
			return err
		}
	}
	return nil
}

func (p Predicate) validate(slug string, invalid func(string) error) error {
	for _, bound := range []*int64{p.MinInputTokens, p.MaxInputTokens, p.MinOutputTokens, p.MaxOutputTokens} {
		if bound != nil && *bound < 0 {
			return invalid("token bounds are non-negative")
		}
	}
	if p.MinInputTokens != nil && p.MaxInputTokens != nil && *p.MinInputTokens > *p.MaxInputTokens ||
		p.MinOutputTokens != nil && p.MaxOutputTokens != nil && *p.MinOutputTokens > *p.MaxOutputTokens {
		return invalid("a minimum exceeds its maximum")
	}
	if len(p.Operations) > maxPredicateList || !distinct(p.Operations) {
		return invalid("operations are distinct")
	}
	if len(p.Modalities) > 0 && !distinctMembers(p.Modalities, Modalities) {
		return invalid("modalities are distinct among text, image, audio, video and file")
	}
	if len(p.ReasoningEffort) > maxPredicateList || !distinct(p.ReasoningEffort) {
		return invalid("reasoning efforts are distinct")
	}
	for _, effort := range p.ReasoningEffort {
		if !effortName.MatchString(effort) {
			return invalid("reasoning efforts are lowercase names")
		}
	}
	if c := p.Classifier; c != nil {
		if !RouteSlug.MatchString(c.Route) || c.Route == slug {
			return invalid("the classifier is another route")
		}
		if len(c.Labels) == 0 || len(c.Labels) > maxPredicateList || !distinct(c.Labels) {
			return invalid("the classifier matches 1 to 16 distinct labels")
		}
		for _, label := range c.Labels {
			if label == "" || len(label) > 128 {
				return invalid("classifier labels are 1 to 128 bytes")
			}
		}
		if c.MinScore != nil && (math.IsNaN(*c.MinScore) || *c.MinScore < 0 || *c.MinScore > 1) {
			return invalid("min_score is between 0 and 1")
		}
		if c.TimeoutMS < 1 || c.TimeoutMS > maxClassifierMS {
			return invalid("the classifier timeout is 1 to 30000 ms")
		}
	}
	if p.Plugin != nil && !pluginDigest.MatchString(p.Plugin.Digest) {
		return invalid("the plugin is named by its lowercase SHA-256 digest")
	}
	return nil
}

// Validate checks spend caps as exact decimals.
func (c CostLimits) Validate(field string) error {
	for _, limit := range []*string{c.DailyCostLimit, c.MonthlyCostLimit} {
		if limit != nil && !decimal(*limit) {
			return access.Invalid(field, "Cost limits are decimals with at most 12 fractional digits")
		}
	}
	if c.DailyCostLimit == nil && c.MonthlyCostLimit == nil {
		return access.Invalid(field, "A budget sets a daily or monthly cost limit")
	}
	return nil
}

// Validate checks a shadow declaration.
func (s Shadow) Validate() error {
	if math.IsNaN(s.SampleRate) || s.SampleRate <= 0 || s.SampleRate > 1 {
		return access.Invalid("targets", "A shadow sample_rate is greater than 0 and at most 1")
	}
	return nil
}

// ValidTargetTags reports whether a target's tags are well formed.
func ValidTargetTags(tags []string) bool {
	if len(tags) > maxTargetTags || !distinct(tags) {
		return false
	}
	for _, tag := range tags {
		if !targetTag.MatchString(tag) {
			return false
		}
	}
	return true
}

// Static reports whether the predicate can be decided from features alone.
func (p Predicate) Static() bool { return p.Classifier == nil && p.Plugin == nil }

// Matches decides the predicate's feature tests. Classifier and plugin tests
// are decided by the caller.
func (p Predicate) Matches(f Features) bool {
	within := func(v int64, lo, hi *int64) bool { return (lo == nil || v >= *lo) && (hi == nil || v <= *hi) }
	switch {
	case len(p.Operations) > 0 && !slices.Contains(p.Operations, f.Operation):
	case !within(f.InputTokens, p.MinInputTokens, p.MaxInputTokens):
	case (p.MinOutputTokens != nil || p.MaxOutputTokens != nil) && (f.OutputTokens == nil || !within(*f.OutputTokens, p.MinOutputTokens, p.MaxOutputTokens)):
	case p.Streaming != nil && *p.Streaming != f.Streaming:
	case p.Tools != nil && *p.Tools != f.Tools:
	case p.StructuredOutput != nil && *p.StructuredOutput != f.StructuredOutput:
	case len(p.ReasoningEffort) > 0 && !slices.Contains(p.ReasoningEffort, f.ReasoningEffort):
	case len(p.Modalities) > 0 && !slices.ContainsFunc(p.Modalities, func(m string) bool { return slices.Contains(f.Modalities, m) }):
	default:
		return true
	}
	return false
}

// Retries returns the retry rule for a failure class.
func (r Retry) Retries(class string) (RetryRule, bool) {
	rule, ok := r[class]
	return rule, ok
}

// Backoff is the delay before the given retry, counted from one: full jitter
// over an exponentially growing ceiling, where u is uniform in [0, 1). A
// stated Retry-After replaces it when the rule honors one.
func (r RetryRule) Backoff(retry int, u float64, retryAfter time.Duration) time.Duration {
	if r.RespectRetryAfter && retryAfter > 0 {
		return retryAfter
	}
	ceiling := float64(r.MaxBackoffMS)
	if retry < 32 {
		ceiling = math.Min(ceiling, float64(r.BaseBackoffMS)*math.Exp2(float64(retry-1)))
	}
	return time.Duration(u * ceiling * float64(time.Millisecond))
}

// Sampled reports whether a shadow target mirrors the request with seed. The
// decision is a pure function of the target and seed, so simulation explains
// the sample a gateway takes for the same request identity.
func Sampled(targetID string, seed []byte, rate float64) bool {
	if rate >= 1 {
		return true
	}
	h := sha256.New()
	h.Write([]byte("olp-shadow-sample\x00"))
	h.Write([]byte(targetID))
	h.Write([]byte{0})
	h.Write(seed)
	u := float64(binary.BigEndian.Uint64(h.Sum(nil)[:8])>>11) / (1 << 53)
	return u < rate
}

// routeEdge is one reference from a route's behavior to another route. A
// serving edge can answer the caller; a classifier edge only labels the
// request.
type routeEdge struct {
	to      string
	serving bool
}

func (b Behavior) edges() []routeEdge {
	var out []routeEdge
	for _, f := range b.Fallbacks {
		out = append(out, routeEdge{f.Route, true})
	}
	for _, s := range b.Selectors {
		if s.Route != "" {
			out = append(out, routeEdge{s.Route, true})
		}
		if s.When.Classifier != nil {
			out = append(out, routeEdge{s.When.Classifier.Route, false})
		}
	}
	return out
}

// References lists the routes a behavior falls back to, delegates to or
// classifies with, each once, in declaration order.
func (b Behavior) References() []string {
	var out []string
	for _, e := range b.edges() {
		if !slices.Contains(out, e.to) {
			out = append(out, e.to)
		}
	}
	return out
}

// CheckRouteGraph refuses fallback, delegation and classifier edges that
// form a cycle, cross a project boundary, lead a strict route to a
// transformed one or chain more than MaxRouteDepth serving routes. References
// to routes that are not published are left to the request path, which
// records them as unavailable.
func CheckRouteGraph(routes map[string]Route) error {
	slugs := slices.Sorted(maps.Keys(routes))
	for _, slug := range slugs {
		r := routes[slug]
		for _, e := range r.edges() {
			next, ok := routes[e.to]
			if !ok {
				continue
			}
			if !sameProject(r.ProjectID, next.ProjectID) {
				return &Refusal{Code: "route_graph_project_mismatch", Message: "Route `" + r.Slug + "` refers to `" + e.to + "` in another project."}
			}
			if e.serving && r.Fidelity.Strict() && !next.Fidelity.Strict() {
				return &Refusal{Code: "route_graph_fidelity_mismatch", Message: "Strict route `" + r.Slug + "` may fall back or delegate only to strict routes; `" + e.to + "` is transformed."}
			}
		}
	}
	// depth is the number of serving routes on the longest chain from a route;
	// state 1 marks a route on the current path, so revisiting it is a cycle.
	state := map[string]int{}
	depth := map[string]int{}
	var visit func(string) error
	visit = func(slug string) error {
		switch state[slug] {
		case 1:
			return &Refusal{Code: "route_graph_cycle", Message: "Route `" + slug + "` reaches itself through fallbacks, selectors or classifiers."}
		case 2:
			return nil
		}
		state[slug] = 1
		longest := 0
		for _, e := range routes[slug].edges() {
			if _, ok := routes[e.to]; !ok {
				continue
			}
			if err := visit(e.to); err != nil {
				return err
			}
			if e.serving {
				longest = max(longest, depth[e.to])
			}
		}
		state[slug], depth[slug] = 2, longest+1
		if depth[slug] > MaxRouteDepth {
			return &Refusal{Code: "route_graph_too_deep", Message: "Route `" + slug + "` chains more than " + strconv.Itoa(MaxRouteDepth) + " routes through fallbacks and selectors."}
		}
		return nil
	}
	for _, slug := range slugs {
		if err := visit(slug); err != nil {
			return err
		}
	}
	return nil
}

func sameProject(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func distinct(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

func distinctMembers(values, allowed []string) bool {
	if !distinct(values) {
		return false
	}
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			return false
		}
	}
	return true
}
