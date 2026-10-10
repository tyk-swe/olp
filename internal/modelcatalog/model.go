// Package modelcatalog projects published routes into a consumer-facing catalog.
package modelcatalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type Range struct {
	Minimum string `json:"minimum"`
	Maximum string `json:"maximum"`
}

type Price struct {
	Operation                   string `json:"operation"`
	Currency                    string `json:"currency"`
	InputPerMillion             *Range `json:"input_per_million"`
	CachedInputPerMillion       *Range `json:"cached_input_per_million"`
	CacheWriteInputPerMillion   *Range `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion *Range `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion *Range `json:"cache_write_1h_input_per_million"`
	OutputPerMillion            *Range `json:"output_per_million"`
	UnitPrice                   *Range `json:"unit_price"`
	Complete                    bool   `json:"complete"`
}

type Privacy struct {
	DataCollection    *bool    `json:"data_collection"`
	ZeroDataRetention *bool    `json:"zero_data_retention"`
	Regions           []string `json:"regions"`
	Unknown           []string `json:"unknown"`
}

type Sample struct {
	SDK       string `json:"sdk"`
	Operation string `json:"operation"`
	Language  string `json:"language"`
	Code      string `json:"code"`
}

type Model struct {
	ID             string                    `json:"id"`
	Capabilities   runtime.RouteCapabilities `json:"capabilities"`
	Privacy        Privacy                   `json:"privacy"`
	Prices         []Price                   `json:"prices,omitempty"`
	UpstreamModels []string                  `json:"upstream_models,omitempty"`
	Samples        []Sample                  `json:"samples"`
}

// Describe exposes only the route identity by default. Prices are consumer
// ranges, rather than provider records; upstream names require a separate opt-in.
func Describe(snapshot *runtime.Snapshot, route runtime.Route, inputs *usage.RoutingInputs, origin string, prices, upstream bool, now time.Time) Model {
	model := Model{ID: route.Slug, Capabilities: runtime.EffectiveCapabilities(snapshot, route), Samples: certifiedSamples(snapshot, route, origin)}
	metadata := []runtime.ModelMetadata{}
	for _, target := range route.Targets {
		provider, ok := snapshot.Providers[target.ProviderID]
		if !ok || !provider.Enabled {
			continue
		}
		var facts runtime.ModelMetadata
		_ = json.Unmarshal(provider.Models[target.ProviderModel], &facts)
		metadata = append(metadata, facts)
		if upstream {
			model.UpstreamModels = append(model.UpstreamModels, target.ProviderModel)
		}
	}
	model.Privacy = privacy(metadata)
	if upstream {
		slices.Sort(model.UpstreamModels)
		model.UpstreamModels = slices.Compact(model.UpstreamModels)
	}
	if prices {
		model.Prices = priceRanges(snapshot, route, inputs, now)
	}
	return model
}

func privacy(metadata []runtime.ModelMetadata) Privacy {
	result := Privacy{Regions: []string{}, Unknown: []string{}}
	collectionKnown, retentionKnown, regionsKnown := len(metadata) > 0, len(metadata) > 0, len(metadata) > 0
	collection, zeroRetention := false, true
	for _, facts := range metadata {
		if facts.DataCollection == nil {
			collectionKnown = false
		} else {
			collection = collection || *facts.DataCollection
		}
		if facts.ZeroDataRetention == nil {
			retentionKnown = false
		} else {
			zeroRetention = zeroRetention && *facts.ZeroDataRetention
		}
		if facts.Region == nil || *facts.Region == "" {
			regionsKnown = false
		} else {
			result.Regions = append(result.Regions, *facts.Region)
		}
	}
	if collectionKnown || collection {
		result.DataCollection = &collection
	} else {
		result.Unknown = append(result.Unknown, "data_collection")
	}
	if retentionKnown || !zeroRetention {
		result.ZeroDataRetention = &zeroRetention
	} else {
		result.Unknown = append(result.Unknown, "zero_data_retention")
	}
	if !regionsKnown {
		result.Unknown = append(result.Unknown, "regions")
	}
	slices.Sort(result.Regions)
	result.Regions = slices.Compact(result.Regions)
	return result
}

func extend(bounds **Range, value *string) {
	if value == nil {
		return
	}
	rate, err := decimal.NewFromString(*value)
	if err != nil || rate.IsNegative() {
		return
	}
	if *bounds == nil {
		*bounds = &Range{rate.String(), rate.String()}
		return
	}
	min, _ := decimal.NewFromString((*bounds).Minimum)
	max, _ := decimal.NewFromString((*bounds).Maximum)
	if rate.LessThan(min) {
		(*bounds).Minimum = rate.String()
	}
	if rate.GreaterThan(max) {
		(*bounds).Maximum = rate.String()
	}
}

func priceRanges(snapshot *runtime.Snapshot, route runtime.Route, inputs *usage.RoutingInputs, now time.Time) []Price {
	result := []Price{}
	for _, operation := range route.Operations {
		byCurrency := map[string]*Price{}
		missing := false
		for _, target := range route.Targets {
			if target.Shadow != nil {
				continue
			}
			provider, ok := snapshot.Providers[target.ProviderID]
			if !ok || !provider.Enabled {
				continue
			}
			price := inputs.Price(provider.Kind, provider.ID, provider.VendorID, target.ProviderModel, operation, now)
			if price == nil {
				missing = true
				continue
			}
			bounds := byCurrency[price.Currency]
			if bounds == nil {
				bounds = &Price{Operation: operation, Currency: price.Currency, Complete: true}
				byCurrency[price.Currency] = bounds
			}
			extend(&bounds.InputPerMillion, price.InputPerMillion)
			cached := price.CachedInputPerMillion
			if cached == nil {
				cached = price.InputPerMillion
			}
			extend(&bounds.CachedInputPerMillion, cached)
			extend(&bounds.CacheWriteInputPerMillion, price.CacheWriteInputPerMillion)
			extend(&bounds.CacheWrite5MInputPerMillion, price.CacheWrite5MInputPerMillion)
			extend(&bounds.CacheWrite1HInputPerMillion, price.CacheWrite1HInputPerMillion)
			extend(&bounds.OutputPerMillion, price.OutputPerMillion)
			extend(&bounds.UnitPrice, price.UnitPrice)
		}
		for _, bounds := range byCurrency {
			bounds.Complete = !missing
			result = append(result, *bounds)
		}
	}
	slices.SortFunc(result, func(a, b Price) int {
		if order := strings.Compare(a.Operation, b.Operation); order != 0 {
			return order
		}
		return strings.Compare(a.Currency, b.Currency)
	})
	return result
}

// Samples use a certified unary tuple. A strict generation example must also
// speak its target's pinned native dialect, rather than imply translation.
func certifiedSamples(snapshot *runtime.Snapshot, route runtime.Route, origin string) []Sample {
	result := []Sample{}
	for _, candidate := range samples(route, origin) {
		surface := candidate.SDK
		if candidate.Operation == "embeddings" {
			surface = "openai"
		}
		if candidate.SDK == "gemini" && candidate.Operation == "embeddings" && !route.Fidelity.Strict() {
			continue
		}
		for _, target := range route.Targets {
			provider, ok := snapshot.Providers[target.ProviderID]
			if !ok || !provider.Enabled || target.Shadow != nil || !provider.Supports(target.ProviderModel, candidate.Operation, surface, "unary") {
				continue
			}
			sample := candidate
			if route.Fidelity.Strict() && candidate.Operation == "generation" {
				profile, err := provider.Connector().Profile()
				if err != nil {
					continue
				}
				var want string
				switch profile.Dialect {
				case "openai-chat", "openai-responses":
					want = "openai"
				case "anthropic-messages":
					want = "anthropic"
				case "gemini-generate-content":
					want = "gemini"
				}
				if candidate.SDK != want {
					continue
				}
				if profile.Dialect == "openai-responses" {
					prefix, _, found := strings.Cut(sample.Code, "response = ")
					if !found {
						continue
					}
					sample.Code = prefix + fmt.Sprintf("response = client.responses.create(model=%s, input=\"Hello\", store=False)\n", strconv.Quote(route.Slug))
				}
			}
			result = append(result, sample)
			break
		}
	}
	return result
}

func samples(route runtime.Route, origin string) []Sample {
	result := []Sample{}
	base, model := strings.TrimSuffix(origin, "/"), strconv.Quote(route.Slug)
	openai := fmt.Sprintf("import os\nfrom openai import OpenAI\n\nclient = OpenAI(api_key=os.environ[\"OLP_API_KEY\"], base_url=%s)\n", strconv.Quote(base+"/v1"))
	anthropic := fmt.Sprintf("import os\nfrom anthropic import Anthropic\n\nclient = Anthropic(api_key=os.environ[\"OLP_API_KEY\"], base_url=%s)\n", strconv.Quote(base+"/anthropic"))
	gemini := fmt.Sprintf("import os\nfrom google import genai\nfrom google.genai import types\n\nclient = genai.Client(api_key=os.environ[\"OLP_API_KEY\"], http_options=types.HttpOptions(base_url=%s, api_version=\"v1beta\"))\n", strconv.Quote(base+"/gemini"))
	if slices.Contains(route.Operations, "embeddings") {
		result = append(result, Sample{SDK: "openai", Operation: "embeddings", Language: "python", Code: openai + fmt.Sprintf("response = client.embeddings.create(model=%s, input=[\"Hello\"])\n", model)}, Sample{SDK: "gemini", Operation: "embeddings", Language: "python", Code: gemini + fmt.Sprintf("response = client.models.embed_content(model=%s, contents=\"Hello\")\n", model)})
	}
	if slices.Contains(route.Operations, "token_count") {
		result = append(result, Sample{SDK: "anthropic", Operation: "token_count", Language: "python", Code: anthropic + fmt.Sprintf("response = client.messages.count_tokens(model=%s, messages=[{\"role\": \"user\", \"content\": \"Hello\"}])\n", model)}, Sample{SDK: "gemini", Operation: "token_count", Language: "python", Code: gemini + fmt.Sprintf("response = client.models.count_tokens(model=%s, contents=\"Hello\")\n", model)})
	}
	if !slices.Contains(route.Operations, "generation") {
		return result
	}
	for _, sample := range []struct{ sdk, code string }{
		{"openai", openai + fmt.Sprintf("response = client.chat.completions.create(model=%s, messages=[{\"role\": \"user\", \"content\": \"Hello\"}])\n", model)},
		{"anthropic", anthropic + fmt.Sprintf("response = client.messages.create(model=%s, max_tokens=256, messages=[{\"role\": \"user\", \"content\": \"Hello\"}])\n", model)},
		{"gemini", gemini + fmt.Sprintf("response = client.models.generate_content(model=%s, contents=\"Hello\")\n", model)},
	} {
		result = append(result, Sample{SDK: sample.sdk, Operation: "generation", Language: "python", Code: sample.code})
	}
	return result
}
