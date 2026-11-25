package routing

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type durableContractConfig struct {
	Provider                      connectors.Config
	ProviderID, RevisionID, Model string
}

type DurableTemplate struct {
	provider connectors.Config
	profile  connectors.Profile
	model    string
	serving  oif.ServingIdentity
}

func durableRefusal(field, requirement, message string) error {
	return operations.Error("target_capability", field, requirement, message)
}

func compileDurableContract(c durableContractConfig) (*DurableTemplate, error) {
	c.Provider = c.Provider.Clone()
	p, err := c.Provider.Profile()
	if err != nil || c.Provider.ValidateProfile() != nil {
		return nil, durableRefusal("/profile", "explicit_profile", "Strict batch requires a registered provider profile.")
	}
	if !slices.Contains(p.Operations, "batch") || !slices.Contains([]string{"direct-openai", "azure-deployment", "azure-v1"}, p.Hosting) || !c.Provider.Supports("batch", "openai", "unary") {
		return nil, durableRefusal("/profile", "native_batch_contract", "This provider profile has no strict batch contract.")
	}
	if !c.Provider.ValidModel(c.Provider.Model(c.Model)) {
		return nil, durableRefusal("/model", "serving_binding", "The batch serving model is invalid.")
	}
	defaults, _, err := c.Provider.DefaultsFor("batch", c.Model)
	if err != nil || len(defaults) != 0 {
		return nil, durableRefusal("/defaults", "native_defaults", "Strict batch has no qualified omission defaults.")
	}
	serving := c.Provider.ServingIdentity(c.ProviderID, c.RevisionID, c.Model)
	return &DurableTemplate{provider: c.Provider, profile: p, model: c.Provider.Model(c.Model), serving: serving}, nil
}

func (t *DurableTemplate) Model() string                { return t.model }
func (t *DurableTemplate) Serving() oif.ServingIdentity { return t.serving }

type BoundDurableBatch struct {
	Source    oif.Request
	Effective oif.Request
	Receipt   oif.Receipt
}

func (t *DurableTemplate) BindBatch(source oif.Document, route, localFile, upstreamFile string) (BoundDurableBatch, error) {
	if source.Root().Kind() != oif.Object {
		return BoundDurableBatch{}, durableRefusal("/request", "native_batch_source", "A batch request must be one JSON object.")
	}
	for _, name := range []string{"id", "output_file_id", "error_file_id", "status", "request_counts"} {
		if _, ok := source.Root().Lookup(name); ok {
			return BoundDurableBatch{}, durableRefusal("/"+name, "provider_owned_field", "The batch request supplies a provider-owned result field.")
		}
	}
	file, ok := source.Root().Lookup("input_file_id")
	value, valid := file.Text()
	if !ok || !valid || value != localFile {
		return BoundDurableBatch{}, durableRefusal("/input_file_id", "owned_file", "The batch input must name the authorized uploaded file.")
	}
	endpoint, ok := source.Root().Lookup("endpoint")
	path, valid := endpoint.Text()
	classificationPath := path
	if t.provider.BatchesByDeployment() {
		classificationPath = wire.CanonicalBatchEndpoint(path)
	}
	if !ok || !valid || !slices.Contains([]string{"/v1/chat/completions", "/v1/responses", "/v1/embeddings"}, classificationPath) {
		return BoundDurableBatch{}, durableRefusal("/endpoint", "native_batch_endpoint", "This batch endpoint has no qualified strict item contract.")
	}
	window, ok := source.Root().Lookup("completion_window")
	period, valid := window.Text()
	if !ok || !valid || period != "24h" {
		return BoundDurableBatch{}, durableRefusal("/completion_window", "native_batch_window", "Strict batch requires the qualified 24h completion window.")
	}
	d := oif.Descriptor{Operation: oif.Identity{ID: "batch", Revision: "1"}, Dialect: oif.Identity{ID: "openai-batch", Revision: t.profile.DialectRevision}, Profile: oif.Identity{ID: t.profile.ID, Revision: t.profile.Revision}, Client: oif.Identity{ID: "openai-batch", Revision: "1"}, Execution: oif.Execution{Delivery: "unary", Lifetime: "durable", Submission: "batch", Effects: []string{"inference", "resource_mutation"}}, Resources: []oif.Resource{{ID: localFile, Kind: "file", Relation: "batch_input"}}}
	original, err := oif.NewRequest(d, source)
	if err != nil {
		return BoundDurableBatch{}, err
	}
	encoded, _ := json.Marshal(upstreamFile)
	changes := []oif.Change{{Pointer: "/input_file_id", Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped uploaded file"}}
	if model, present := source.Root().Lookup("model"); present {
		text, valid := model.Text()
		if !valid || text != route && text != t.model {
			return BoundDurableBatch{}, durableRefusal("/model", "serving_binding", "The batch model differs from the selected provider binding.")
		}
		if text != t.model {
			encoded, _ = json.Marshal(t.model)
			changes = append(changes, oif.Change{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "selected provider model"})
		}
	} else if t.provider.BatchesByDeployment() {
		encoded, _ = json.Marshal(t.model)
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "selected Azure deployment"})
	}
	effective, err := original.WithChanges(changes...)
	if err != nil {
		return BoundDurableBatch{}, err
	}
	receipt := oif.Receipt{Class: "native_identity", Operation: "batch", SourceDialect: d.Dialect.ID, TargetDialect: d.Dialect.ID, ProfileID: t.profile.ID, ProfileRevision: t.profile.Revision, Serving: t.serving,
		Evidence: []string{"durable/native-openai/1", t.profile.Documentation}, Obligations: oif.Obligations{Delivery: "unary", Lifetime: "durable", Submission: "batch", Continuation: "owner_scoped_resource", Effects: []string{"inference", "resource_mutation"}, Retry: "no_retry_after_outcome_unknown", MaxBodyBytes: 4 << 20, MaxContinuationBytes: 4 << 20, RejectAmbiguousFailover: true, GuardResults: true}}
	for _, change := range changes {
		receipt.Dispositions = append(receipt.Dispositions, oif.Disposition{Field: change.Pointer, Disposition: "bound", Rule: change.Reason, Evidence: "durable/native-openai/1"})
	}
	return BoundDurableBatch{Source: original, Effective: effective, Receipt: receipt}, nil
}
