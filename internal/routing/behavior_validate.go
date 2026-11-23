package routing

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	pluginidentity "github.com/tyk-swe/olp/internal/domain/pluginidentity"
	"github.com/tyk-swe/olp/internal/domain/pricingpolicy"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/upstream"
)

// Validate checks the behavior's own shape. tagged reports whether a primary
// target of the route carries a tag, so a selector cannot steer to nothing.
func (b Behavior) Validate(slug string, tagged func(string) bool) error {
	if b.RunLimits != nil && (!b.RunLimits.Valid() || b.ToolExecution != "gateway") {
		return invalidValue("run_limits", "Use valid workflow limits on a gateway tool route.")
	}
	if b.ToolExecution != "" && b.ToolExecution != "gateway" {
		return invalidValue("tool_execution", "Use gateway or omit tool execution.")
	}
	if len(b.Toolsets) > 0 && b.ToolExecution != "gateway" {
		return invalidValue("toolsets", "Toolsets require gateway tool execution.")
	}
	if b.ResponsesMCPItems && b.ToolExecution != "gateway" {
		return invalidValue("responses_mcp_items", "Responses MCP items require gateway tool execution.")
	}
	for index, id := range b.Toolsets {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || slices.Index(b.Toolsets, id) != index {
			return invalidValue("toolsets", "Use distinct canonical toolset IDs.")
		}
	}
	if err := b.Cache.Validate(); err != nil {
		return invalidValue("cache", err.Error())
	}
	if err := b.PromptCache.Validate(); err != nil {
		return invalidValue("prompt_cache", err.Error())
	}
	if !bodylimit.Valid(b.MaxBodyBytes) {
		return invalidValue("max_body_bytes", "Use a body limit from 1 to 1073741824 bytes, or null to inherit the installation limit.")
	}
	if len(b.Fallbacks) > maxFallbacks {
		return invalidValue("fallbacks", "A route declares at most "+strconv.Itoa(maxFallbacks)+" fallbacks")
	}
	fallbackConditions := []string{
		FallbackExhausted, FallbackContextWindow, FallbackContentFilter, FallbackRateLimit, FallbackBudget,
	}
	seen := map[string]bool{}
	for _, f := range b.Fallbacks {
		if !routeidentity.ValidSlug(f.Route) || f.Route == slug {
			return invalidValue("fallbacks", "Each fallback names another route by slug")
		}
		if seen[f.Route] {
			return invalidValue("fallbacks", "Each fallback route appears once")
		}
		seen[f.Route] = true
		if len(f.On) == 0 || !distinctMembers(f.On, fallbackConditions) {
			return invalidValue("fallbacks", "Each fallback names distinct conditions among "+
				strings.Join(fallbackConditions[:len(fallbackConditions)-1], ", ")+" and "+fallbackConditions[len(fallbackConditions)-1])
		}
	}
	if err := b.validateSelectors(slug, tagged); err != nil {
		return err
	}
	retryClasses := []string{
		string(upstream.Connect), string(upstream.Timeout), string(upstream.RateLimit), string(upstream.ServerError),
	}
	for class, rule := range b.Retry {
		if !slices.Contains(retryClasses, class) {
			return invalidValue("retry", "Retries apply to "+
				strings.Join(retryClasses[:len(retryClasses)-1], ", ")+" and "+retryClasses[len(retryClasses)-1]+" failures")
		}
		if rule.MaxRetries < 1 || rule.MaxRetries > maxRetries || rule.BaseBackoffMS < 1 || rule.MaxBackoffMS < rule.BaseBackoffMS || rule.MaxBackoffMS > maxBackoffMS {
			return invalidValue("retry", "Each retry rule allows 1 to 10 retries with a base backoff no larger than its maximum of at most 60000 ms")
		}
	}
	if a := b.Affinity; a != nil {
		switch {
		case a.Source == AffinityLabel && attribution.ValidKey(a.Label):
		case a.Source == AffinityCacheKey && a.Label == "":
		default:
			return invalidValue("affinity", "Affinity reads an attribution label or the dialect's cache key")
		}
	}
	if b.Budget != nil {
		if err := b.Budget.Validate("budget"); err != nil {
			return err
		}
	}
	return nil
}

func (b Behavior) validateSelectors(slug string, tagged func(string) bool) error {
	if len(b.Selectors) > maxSelectors {
		return invalidValue("selectors", "A route declares at most "+strconv.Itoa(maxSelectors)+" selectors")
	}
	ids := map[string]bool{}
	for _, s := range b.Selectors {
		invalid := func(message string) error { return invalidValue("selectors", "Selector `"+s.ID+"`: "+message) }
		if !routeidentity.ValidSelectorID(s.ID) || ids[s.ID] {
			return invalidValue("selectors", "Each selector has a distinct lowercase identifier")
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
			if !routeidentity.ValidSlug(s.Route) || s.Route == slug {
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
	if len(p.Modalities) > 0 && !distinctMembers(p.Modalities, protocols.RequestModalities()) {
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
		if !routeidentity.ValidSlug(c.Route) || c.Route == slug {
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
	if p.Plugin != nil && !pluginidentity.ValidDigest(p.Plugin.Digest) {
		return invalid("the plugin is named by its lowercase SHA-256 digest")
	}
	return nil
}

// Validate checks spend caps as positive exact decimals.
func (c CostLimits) Validate(field string) error {
	for _, limit := range []*string{c.DailyCostLimit, c.MonthlyCostLimit} {
		if limit != nil && (!pricingpolicy.ValidAmount(*limit) || strings.Trim(*limit, "0.") == "") {
			return invalidValue(field, "Cost limits are positive decimals with at most 12 fractional digits")
		}
	}
	if c.DailyCostLimit == nil && c.MonthlyCostLimit == nil {
		return invalidValue(field, "A budget sets a daily or monthly cost limit")
	}
	return nil
}

// Validate checks a shadow declaration.
func (s Shadow) Validate() error {
	if math.IsNaN(s.SampleRate) || s.SampleRate <= 0 || s.SampleRate > 1 {
		return invalidValue("targets", "A shadow sample_rate is greater than 0 and at most 1")
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
