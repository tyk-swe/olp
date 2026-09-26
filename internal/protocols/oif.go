package protocols

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

var contracts = buildContracts()

func buildContracts() *oif.Registry {
	families := []openai.Family{openai.FamilyChat, openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini, openai.FamilyBedrock, openai.FamilyInputTokens, openai.FamilyAnthropicCount, openai.FamilyGeminiCount, "bedrock_count", openai.FamilyEmbeddings, openai.FamilyGeminiEmbeddings, openai.FamilyGeminiEmbeddingsBatch, openai.FamilyVertexEmbeddings, openai.FamilyBedrockEmbeddings, openai.FamilyRerank, openai.FamilyModeration}
	operations := []oif.Operation{}
	bindings := []oif.Binding{}
	seen := map[oif.Identity]bool{}
	for _, family := range families {
		d := openai.Descriptor(family, false)
		if !seen[d.Operation] {
			operations = append(operations, oif.Operation{Identity: d.Operation})
			seen[d.Operation] = true
		}
		binding := oif.Binding{Dialect: d.Dialect, Operation: d.Operation}
		if family.Surface() != "gemini" && family != openai.FamilyBedrock && family != "bedrock_count" {
			binding.IdentityRules = append(binding.IdentityRules, oif.IdentityRule{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String})
		}
		if family == openai.FamilyGeminiCount {
			binding.IdentityRules = append(binding.IdentityRules, oif.IdentityRule{Pointer: "/generateContentRequest/model", Origin: oif.IdentityBinding, Kind: oif.String})
		}
		if family == openai.FamilyChat {
			binding.IdentityRules = append(binding.IdentityRules,
				oif.IdentityRule{Pointer: "/stream_options", Origin: oif.TransportOption, Kind: oif.Object, Validate: func(before, after oif.Value) bool {
					return (before.Kind() == oif.Absent || before.Kind() == oif.Null) && len(after.Members()) == 1 && field(after, "include_usage").Raw() == "true"
				}},
				oif.IdentityRule{Pointer: "/stream_options/include_usage", Origin: oif.TransportOption, Kind: oif.Boolean, Validate: func(_, after oif.Value) bool { return after.Raw() == "true" }})
		}
		if family == openai.FamilyResponses {
			binding.IdentityRules = append(binding.IdentityRules, oif.IdentityRule{Pointer: "/previous_response_id", Origin: oif.ResourceBinding, Kind: oif.String})
		}
		if d.Operation == generation.Contract() {
			adapter := generationAdapter{family}
			binding.Request = adapter
			binding.Result = adapter
			binding.Event = adapter
		}
		bindings = append(bindings, binding)
	}
	r, err := oif.NewRegistry(operations, bindings)
	if err != nil {
		panic(err)
	}
	return r
}

// PrepareTarget is the translation path for transformed routes. The
// destination source and transformed provenance make its changes explicit;
// strict planners use PrepareIdentity or an independently qualified lowering.
func PrepareTarget(r *openai.Request, wire openai.Family, kind, vendor, model string, defaults Object) (oif.Prepared, openai.Family, error) {
	destination := openai.Descriptor(wire, r.Stream)
	if destination.Operation != r.OIF().Descriptor().Operation {
		return oif.Prepared{}, wire, unsupported("operation")
	}
	if _, err := contracts.Binding(destination.Dialect, destination.Operation); err != nil {
		return oif.Prepared{}, wire, unsupported("operation")
	}
	var applied []oif.Provenance
	body, wire, err := encodeTransformed(r, wire, kind, vendor, model, defaults, &applied)
	if err != nil {
		return oif.Prepared{}, wire, err
	}
	// An operation codec may select a documented collection form such as
	// Gemini batch embeddings. The result descriptor must name that actual wire.
	destination = openai.Descriptor(wire, r.Stream)
	if _, err := contracts.Binding(destination.Dialect, destination.Operation); err != nil {
		return oif.Prepared{}, wire, unsupported("operation")
	}
	doc, err := oif.ParseJSON(body, r.OIF().Document().Limits())
	if err != nil {
		return oif.Prepared{}, wire, err
	}
	p, err := oif.PrepareDestination(r.OIF(), destination, doc, oif.TransformedMapping, "transformed route codec")
	slices.SortFunc(applied, func(a, b oif.Provenance) int { return strings.Compare(a.Pointer, b.Pointer) })
	p = p.WithProvenance(applied...)
	return p, wire, err
}

// PrepareIdentity binds only the configured model and necessary transport
// accounting option. It does not rename token controls, normalize input forms,
// drop native fields, or assume a different dialect executes foreign state.
func PrepareIdentity(r *openai.Request, model string) (oif.Prepared, error) {
	d := r.OIF().Descriptor()
	if _, err := contracts.Binding(d.Dialect, d.Operation); err != nil {
		return oif.Prepared{}, err
	}
	changes := []oif.Change{}
	if r.Family.Surface() != "gemini" && r.Family != openai.FamilyBedrock && r.Family != "bedrock_count" {
		encoded, _ := json.Marshal(model)
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published route model binding"})
	}
	if r.Family == openai.FamilyGeminiCount {
		if nested, _ := r.OIF().Document().Lookup("/generateContentRequest"); nested.Kind() == oif.Object {
			encoded, _ := json.Marshal("models/" + strings.TrimPrefix(model, "models/"))
			changes = append(changes, oif.Change{Pointer: "/generateContentRequest/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published route model binding"})
		}
	}
	if r.Stream && r.Family == openai.FamilyChat {
		path, value := "/stream_options", `{"include_usage":true}`
		if options, _ := r.OIF().Document().Lookup("/stream_options"); options.Kind() == oif.Object {
			path, value = "/stream_options/include_usage", "true"
		}
		changes = append(changes, oif.Change{Pointer: path, Value: value, Origin: oif.TransportOption, Reason: "observe native usage for shared accounting"})
	}
	return contracts.PrepareIdentity(r.OIF(), d, changes)
}

func GenerationRequest(r *openai.Request) (generation.Request, error) {
	d := r.OIF().Descriptor()
	binding, err := contracts.Binding(d.Dialect, d.Operation)
	if err != nil || binding.Request == nil {
		return generation.Request{}, fmt.Errorf("generation request view unavailable")
	}
	view, err := binding.Request.LiftRequest(r.OIF())
	if err != nil {
		return generation.Request{}, err
	}
	return view.(generation.Request), nil
}
func GenerationResult(result oif.Result) (generation.Result, error) {
	d := result.Descriptor()
	binding, err := contracts.Binding(d.Dialect, d.Operation)
	if err != nil || binding.Result == nil {
		return generation.Result{}, fmt.Errorf("generation result view unavailable")
	}
	view, err := binding.Result.LiftResult(result)
	if err != nil {
		return generation.Result{}, err
	}
	return view.(generation.Result), nil
}
