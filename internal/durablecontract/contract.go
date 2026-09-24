// Package durablecontract owns the strict native file/batch contract. Resource
// persistence, provider dispatch, and quota accounting remain with their
// existing owners.
package durablecontract

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

type Config struct {
	Provider                      connectors.Config
	ProviderID, RevisionID, Model string
	Policy                        *contentpolicy.Policy
}

type Template struct {
	provider connectors.Config
	profile  connectors.Profile
	model    string
	serving  oif.ServingIdentity
}

func refusal(field, requirement, message string) error {
	return operations.Error("target_capability", field, requirement, message)
}

// Compile fails activation for unreviewed batch hosting/default/policy
// combinations. A file resource is admitted only under this batch target.
func Compile(c Config) (*Template, error) {
	p, err := c.Provider.Profile()
	if err != nil || c.Provider.ValidateProfile() != nil {
		return nil, refusal("/profile", "explicit_profile", "Strict batch requires a registered provider profile.")
	}
	if !slices.Contains(p.Operations, "batch") || !slices.Contains([]string{"direct-openai", "azure-deployment", "azure-v1"}, p.Hosting) || !c.Provider.Supports("batch", "openai", "unary") {
		return nil, refusal("/profile", "native_batch_contract", "This provider profile has no strict batch contract.")
	}
	if !connectors.ModelValid(c.Provider.Kind, c.Provider.Model(c.Model)) {
		return nil, refusal("/model", "serving_binding", "The batch serving model is invalid.")
	}
	defaults, _, err := c.Provider.DefaultsFor("batch", c.Model)
	if err != nil || len(defaults) != 0 {
		return nil, refusal("/defaults", "native_defaults", "Strict batch has no qualified omission defaults.")
	}
	if contentpolicy.Validate(c.Policy) != nil || c.Policy != nil && len(c.Policy.Rules) != 0 {
		return nil, operations.Error("policy_conflict", "/content_policy", "inspectable_batch", "Strict batch cannot inspect each queued item for this content policy.")
	}
	binding := c.Provider.Bindings[c.Model]
	serving := oif.ServingIdentity{ProviderID: c.ProviderID, RevisionID: c.RevisionID, Model: c.Provider.Model(c.Model), ProfileID: p.ID, ProfileRevision: p.Revision, PrincipalID: binding.PrincipalID, Snapshot: binding.Snapshot, Region: c.Provider.CloudRegion, ResourceScope: binding.ResourceScope}
	if binding.Region != "" {
		serving.Region = binding.Region
	}
	return &Template{provider: c.Provider, profile: p, model: c.Provider.Model(c.Model), serving: serving}, nil
}

func (t *Template) Model() string                { return t.model }
func (t *Template) Serving() oif.ServingIdentity { return t.serving }

type Bound struct {
	Source    oif.Request
	Effective oif.Request
	Receipt   oif.Receipt
}

func (t *Template) BindBatch(source oif.Document, route, localFile, upstreamFile string) (Bound, error) {
	if source.Root().Kind() != oif.Object {
		return Bound{}, refusal("/request", "native_batch_source", "A batch request must be one JSON object.")
	}
	for _, name := range []string{"id", "output_file_id", "error_file_id", "status", "request_counts"} {
		if _, ok := source.Root().Lookup(name); ok {
			return Bound{}, refusal("/"+name, "provider_owned_field", "The batch request supplies a provider-owned result field.")
		}
	}
	file, ok := source.Root().Lookup("input_file_id")
	value, valid := file.Text()
	if !ok || !valid || value != localFile {
		return Bound{}, refusal("/input_file_id", "owned_file", "The batch input must name the authorized uploaded file.")
	}
	endpoint, ok := source.Root().Lookup("endpoint")
	path, valid := endpoint.Text()
	if !ok || !valid || !slices.Contains([]string{"/v1/chat/completions", "/v1/responses", "/v1/embeddings"}, path) {
		return Bound{}, refusal("/endpoint", "native_batch_endpoint", "This batch endpoint has no qualified strict item contract.")
	}
	window, ok := source.Root().Lookup("completion_window")
	period, valid := window.Text()
	if !ok || !valid || period != "24h" {
		return Bound{}, refusal("/completion_window", "native_batch_window", "Strict batch requires the qualified 24h completion window.")
	}
	d := oif.Descriptor{Operation: oif.Identity{ID: "batch", Revision: "1"}, Dialect: oif.Identity{ID: "openai-batch", Revision: t.profile.DialectRevision}, Profile: oif.Identity{ID: t.profile.ID, Revision: t.profile.Revision}, Client: oif.Identity{ID: "openai-batch", Revision: "1"}, Execution: oif.Execution{Delivery: "unary", Lifetime: "durable", Submission: "batch", Effects: []string{"inference", "resource_mutation"}}, Resources: []oif.Resource{{ID: localFile, Kind: "file", Relation: "batch_input"}}}
	original, err := oif.NewRequest(d, source)
	if err != nil {
		return Bound{}, err
	}
	encoded, _ := json.Marshal(upstreamFile)
	changes := []oif.Change{{Pointer: "/input_file_id", Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped uploaded file"}}
	if model, present := source.Root().Lookup("model"); present {
		text, valid := model.Text()
		if !valid || text != route && text != t.model {
			return Bound{}, refusal("/model", "serving_binding", "The batch model differs from the selected provider binding.")
		}
		if text != t.model {
			encoded, _ = json.Marshal(t.model)
			changes = append(changes, oif.Change{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "selected provider model"})
		}
	} else if t.provider.Kind == "azure_openai" {
		encoded, _ = json.Marshal(t.model)
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "selected Azure deployment"})
	}
	effective, err := original.WithChanges(changes...)
	if err != nil {
		return Bound{}, err
	}
	receipt := oif.Receipt{Class: "native_identity", Operation: "batch", SourceDialect: d.Dialect.ID, TargetDialect: d.Dialect.ID, ProfileID: t.profile.ID, ProfileRevision: t.profile.Revision, Serving: t.serving,
		Evidence: []string{"durable/native-openai/1", t.profile.Documentation}, Obligations: oif.Obligations{Delivery: "unary", Lifetime: "durable", Submission: "batch", Continuation: "owner_scoped_resource", Effects: []string{"inference", "resource_mutation"}, Retry: "no_retry_after_outcome_unknown", MaxBodyBytes: 4 << 20, MaxContinuationBytes: 4 << 20, RejectAmbiguousFailover: true, GuardResults: true}}
	for _, change := range changes {
		receipt.Dispositions = append(receipt.Dispositions, oif.Disposition{Field: change.Pointer, Disposition: "bound", Rule: change.Reason, Evidence: "durable/native-openai/1"})
	}
	return Bound{Source: original, Effective: effective, Receipt: receipt}, nil
}
