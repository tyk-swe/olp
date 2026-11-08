package agentpublication

import (
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

func Parse(document contract.ConfigurationDocument, policy *egress.Policy) ([]Publication, error) {
	entries := valueOf(document.AgentPublications)
	if len(entries) > maxPublications {
		return nil, access.Invalid("agent_publications", "Declare at most 512 publications.")
	}
	publications, err := collect(entries, func(index int, entry contract.ConfigurationAgentPublication) (Publication, error) {
		return parseEntry(entry, entries[:index], policy)
	})
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(slices.Values(publications), byKey), nil
}

func parseEntry(entry contract.ConfigurationAgentPublication, prior []contract.ConfigurationAgentPublication, policy *egress.Policy) (Publication, error) {
	key := entryKey(entry)
	if !promotion.GuardrailLabel(entry.Project) || slices.ContainsFunc(prior, func(other contract.ConfigurationAgentPublication) bool { return entryKey(other) == key }) {
		return Publication{}, access.Invalid("agent_publications", "Use unique owning project/kind/name identities.")
	}
	draft, err := promotion.Convert[agentcontract.Draft](entry.Draft)
	if err != nil {
		return Publication{}, err
	}
	published, err := convertOptional(entry.Published)
	if err != nil {
		return Publication{}, err
	}
	labels, err := promotion.Convert[map[string]agentcontract.Draft](valueOf(entry.Labels))
	if err != nil {
		return Publication{}, err
	}
	if err := refuseShape(entry, derivedRef(key), draft, published, labels); err != nil {
		return Publication{}, err
	}
	publication := Publication{Project: entry.Project, Draft: draft, Published: published, Kind: kindOf(string(entry.Kind), nonEmpty(labels), refOf(entry.CredentialRef), refOf(entry.PublishedCredentialRef))}
	for _, definition := range publication.definitions() {
		if err := validateDefinition(definition, string(entry.Kind), policy); err != nil {
			return Publication{}, err
		}
	}
	return publication, nil
}

func refuseShape(entry contract.ConfigurationAgentPublication, ref promotion.Ref, draft agentcontract.Draft, published *agentcontract.Draft, labels map[string]agentcontract.Draft) error {
	agent := entry.Kind == contract.ConfigurationAgentPublicationKindAgent
	if entry.Kind != contract.ConfigurationAgentPublicationKindPrompt && len(labels) != 0 {
		return access.Invalid("agent_publications.labels", "Only prompts have labels.")
	}
	if slices.ContainsFunc(slices.Collect(maps.Keys(labels)), func(label string) bool { return !slices.Contains(labelNames, label) }) {
		return access.Invalid("agent_publications.labels", "Use production or staging.")
	}
	if entry.CredentialRef != nil && (!agent || promotion.Ref(*entry.CredentialRef) != ref) {
		return access.Invalid("agent_publications.credential_ref", "Use the deterministic exported agent credential reference.")
	}
	if entry.PublishedCredentialRef != nil && (!agent || published == nil || !publishedRefValid(promotion.Ref(*entry.PublishedCredentialRef), ref, entry.CredentialRef != nil, draft, *published)) {
		return access.Invalid("agent_publications.published_credential_ref", "Bind published credentials independently; sharing requires the same approved endpoint.")
	}
	return nil
}

func publishedRefValid(published, derived promotion.Ref, drafted bool, draft, definition agentcontract.Draft) bool {
	if published == derived+"/published" {
		return true
	}
	return published == derived && drafted && draft.Agent != nil && definition.Agent != nil && draft.Agent.Endpoint == definition.Agent.Endpoint
}

func validateDefinition(definition agentcontract.Draft, kindName string, policy *egress.Policy) error {
	if err := placed(definition, placeholderProject).Validate(kindName); err != nil {
		return access.Invalid("agent_publications", err.Error())
	}
	if definition.Agent == nil {
		return nil
	}
	if definition.Agent.CredentialID != nil {
		return access.Invalid("agent_publications", "Export credential references, never destination secret UUIDs.")
	}
	if policy == nil {
		return access.Invalid("agent_publications.endpoint", "Egress policy is unavailable.")
	}
	endpoint, err := policy.ValidateEndpoint(definition.Agent.Endpoint)
	if err != nil || endpoint == nil || endpoint.String() != definition.Agent.Endpoint {
		return access.Invalid("agent_publications.endpoint", "Use a canonical approved endpoint.")
	}
	return nil
}

func entryKey(entry contract.ConfigurationAgentPublication) string {
	return keyOf(entry.Project, string(entry.Kind), entry.Draft.Name)
}

func convertOptional(draft *contract.ConfigurationAgentDraft) (*agentcontract.Draft, error) {
	if draft == nil {
		return nil, nil
	}
	converted, err := promotion.Convert[agentcontract.Draft](*draft)
	return &converted, err
}

func placed(definition agentcontract.Draft, project string) agentcontract.Draft {
	return agentcontract.Draft{ProjectID: project, Name: definition.Name, Toolset: definition.Toolset, Agent: definition.Agent, Prompt: definition.Prompt}
}

func nonEmpty[K comparable, V any](values map[K]V) map[K]V {
	if len(values) == 0 {
		return nil
	}
	return values
}

func valueOf[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}

func collect[A, B any](values []A, convert func(int, A) (B, error)) ([]B, error) {
	converted := make([]B, 0, len(values))
	for index, value := range values {
		next, err := convert(index, value)
		if err != nil {
			return nil, err
		}
		converted = append(converted, next)
	}
	return converted, nil
}
