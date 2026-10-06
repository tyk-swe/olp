package providers

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/runtime"
)

// The reference catalog offers its facts beside a provider's models. Accepting
// them stores operator facts tagged with the catalog's digest. That changes
// the provider's configuration, so certified capabilities return to declared:
// the catalog never certifies anything.

// canonicalModel is the canonical model a provider model's facts declare, by
// which the catalog also matches it.
func canonicalModel(metadata json.RawMessage) string {
	var facts struct {
		CanonicalModel string `json:"canonical_model"`
	}
	_ = json.Unmarshal(metadata, &facts)
	return facts.CanonicalModel
}

type catalogSuggestion struct {
	ModelID       string `json:"model_id"`
	UpstreamModel string `json:"upstream_model"`
	CatalogModel  string `json:"catalog_model"`
	MatchedBy     string `json:"matched_by"`
	// Facts are what accepting the suggestion stores.
	Facts runtime.ModelMetadata `json:"facts"`
	// Changes name the stored facts accepting would change.
	Changes      []string               `json:"changes"`
	Capabilities catalog.Capabilities   `json:"capabilities"`
	Lifecycle    *catalog.LifecycleView `json:"lifecycle"`
	// Conflict explains why the suggestion cannot be accepted.
	Conflict *string `json:"conflict"`
}

// catalogFactNames are the facts the catalog states. Deployment, region,
// quantization and privacy declarations describe the operator's account and
// are never taken from it.
var catalogFactNames = []string{"canonical_model", "context_length", "max_output_tokens", "input_modalities", "output_modalities", "supported_parameters"}

// catalogFacts are the operator facts a catalog model supports, tagged with
// the catalog's digest and the time the vendor's page was observed.
func catalogFacts(signed *catalog.Signed, m *catalog.Model) runtime.ModelMetadata {
	facts := runtime.ModelMetadata{InputModalities: m.InputModalities, OutputModalities: m.OutputModalities, ContextLength: m.ContextLength, MaxOutputTokens: m.MaxOutputTokens, SupportedParameters: m.SupportedParameters,
		Source: new(signed.Source()), ObservedAt: new(m.Provenance.ObservedAt)}
	if m.CanonicalModel != "" {
		facts.CanonicalModel = new(m.CanonicalModel)
	}
	return facts
}

// applyCatalogFacts overlays catalog facts on a model's stored facts, keeping
// every fact the catalog does not state, and reports which facts changed.
func applyCatalogFacts(stored json.RawMessage, facts runtime.ModelMetadata) (json.RawMessage, []string, error) {
	current := map[string]json.RawMessage{}
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &current); err != nil {
			return nil, nil, err
		}
	}
	proposed := map[string]json.RawMessage{}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, nil, err
	}
	if err = json.Unmarshal(encoded, &proposed); err != nil {
		return nil, nil, err
	}
	changes := []string{}
	for _, name := range catalogFactNames {
		value := proposed[name]
		if string(value) == "null" {
			continue
		}
		if !sameFact(current[name], value) {
			changes = append(changes, name)
		}
		current[name] = value
	}
	current["source"], current["observed_at"] = proposed["source"], proposed["observed_at"]
	merged, err := json.Marshal(current)
	if err != nil {
		return nil, nil, err
	}
	return merged, changes, validateMetadata(merged)
}

func sameFact(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// privacyEvidence reports facts whose source attests a privacy declaration:
// overwriting their source would make the catalog look like that evidence.
func privacyEvidence(stored json.RawMessage) bool {
	var facts runtime.ModelMetadata
	_ = json.Unmarshal(stored, &facts)
	return facts.DataCollection != nil || facts.ZeroDataRetention != nil
}

// suggestionsFor matches a provider's models against the catalog under the
// provider's vendor.
func suggestionsFor(signed *catalog.Signed, cfg *Configuration, models []storedModel) ([]catalogSuggestion, error) {
	vendorID := value(cfg.Options.VendorID)
	out := []catalogSuggestion{}
	for _, model := range models {
		stored := cfg.Options.Models[model.UpstreamModel]
		match, matchedBy, ok := signed.Lookup(vendorID, model.UpstreamModel, canonicalModel(stored))
		if !ok {
			continue
		}
		facts := catalogFacts(signed, match)
		_, changes, err := applyCatalogFacts(stored, facts)
		if err != nil {
			return nil, err
		}
		suggestion := catalogSuggestion{ModelID: model.ID, UpstreamModel: model.UpstreamModel, CatalogModel: match.ID, MatchedBy: string(matchedBy),
			Facts: facts, Changes: changes, Capabilities: match.Capabilities, Lifecycle: match.Lifecycle.View()}
		if privacyEvidence(stored) {
			suggestion.Conflict = new("privacy_evidence")
		}
		out = append(out, suggestion)
	}
	return out, nil
}

func (s *Server) catalogSuggestions(r *http.Request, p access.Principal) (access.Reply, error) {
	if s.Catalog == nil {
		return access.Reply{}, access.Fail(503, "reference_catalog_unavailable", "This process holds no verified reference catalog.")
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := visibleProvider(r.Context(), s.Access.Pool, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	models, err := loadModels(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := suggestionsFor(s.Catalog, &current.Configuration, models)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string]any{"catalog": s.Catalog.Summary(), "vendor_id": value(current.Configuration.Options.VendorID), "items": items}), nil
}

type acceptCatalogRequest struct {
	CatalogSHA256  string   `json:"catalog_sha256"`
	UpstreamModels []string `json:"upstream_models"`
}

// acceptCatalogSuggestions stores the catalog's facts for the named models as
// operator facts. The caller names the catalog it reviewed, so an upgrade
// between review and acceptance cannot store facts nobody saw.
func (s *Server) acceptCatalogSuggestions(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input acceptCatalogRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if s.Catalog == nil {
		return access.Reply{}, access.Fail(503, "reference_catalog_unavailable", "This process holds no verified reference catalog.")
	}
	if len(input.UpstreamModels) == 0 || len(input.UpstreamModels) > 2000 {
		return access.Reply{}, access.Invalid("upstream_models", "Name 1–2000 models to accept catalog facts for.")
	}
	return s.mutationWith(r, "provider.catalog_facts.accept", input, func(ctx context.Context, tx pgx.Tx, _ access.Principal, current *record) (access.Reply, error) {
		if input.CatalogSHA256 != s.Catalog.SHA256 {
			return access.Reply{}, access.Fail(409, "catalog_changed", "The reference catalog changed since these suggestions were reviewed; review them again.")
		}
		models, err := loadModels(ctx, tx, current.ID, false)
		if err != nil {
			return access.Reply{}, err
		}
		suggestions, err := suggestionsFor(s.Catalog, &current.Configuration, models)
		if err != nil {
			return access.Reply{}, err
		}
		byModel := make(map[string]*catalogSuggestion, len(suggestions))
		for i := range suggestions {
			if _, seen := byModel[suggestions[i].UpstreamModel]; !seen {
				byModel[suggestions[i].UpstreamModel] = &suggestions[i]
			}
		}
		next := current.Configuration
		next.Options.Models = maps.Clone(current.Configuration.Options.Models)
		if next.Options.Models == nil {
			next.Options.Models = map[string]json.RawMessage{}
		}
		for _, name := range input.UpstreamModels {
			suggestion := byModel[name]
			switch {
			case suggestion == nil:
				return access.Reply{}, access.Fail(422, "catalog_model_unmatched", "The reference catalog has no facts for "+name+".")
			case suggestion.Conflict != nil:
				return access.Reply{}, access.Fail(422, "privacy_evidence", "The facts of "+name+" attest a privacy declaration; keep them and edit the model's facts instead.")
			}
			merged, _, err := applyCatalogFacts(next.Options.Models[name], suggestion.Facts)
			if err != nil {
				return access.Reply{}, err
			}
			next.Options.Models[name] = merged
		}
		if err = s.storeDraft(ctx, tx, current.ID, current.Name, &current.Configuration, &next); err != nil {
			return access.Reply{}, err
		}
		return s.detailReply(ctx, tx, current.ID)
	})
}
