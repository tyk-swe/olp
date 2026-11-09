package agentpublication

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

func Read(ctx context.Context, q access.Queryer) ([]Publication, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	records, err := publications.ReadActive(ctx, q)
	if err != nil {
		return nil, err
	}
	labels, err := publications.ReadActivePromptLabels(ctx, q)
	if err != nil {
		return nil, err
	}
	exports, err := promotion.TryMap(records, func(record publications.ActivePublication) (Publication, error) {
		name, ok := directory.Name(record.ProjectID)
		if !ok {
			return Publication{}, fmt.Errorf("agent publication %s belongs to an unknown project", record.ID)
		}
		return exported(record, name, labelsOf(labels, record.ID)), nil
	})
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(slices.Values(exports), byKey), nil
}

func labelsOf(labels []publications.PromptLabel, publication string) map[string]agentcontract.Draft {
	return maps.Collect(func(yield func(string, agentcontract.Draft) bool) {
		for _, label := range labels {
			if label.PublicationID == publication && !yield(label.Label, exportDraft(label.Definition)) {
				return
			}
		}
	})
}

func exported(record publications.ActivePublication, projectName string, labels map[string]agentcontract.Draft) Publication {
	ref := derivedRef(keyOf(projectName, record.Kind, record.Draft.Name))
	credential := credentialRef(record.Draft, ref)
	return Publication{
		Project:   projectName,
		Draft:     exportDraft(record.Draft),
		Published: exportOptional(record.Published),
		Kind:      kindOf(record.Kind, labels, credential, publishedRef(record, ref, credential)),
	}
}

func credentialRef(draft agentcontract.Draft, ref promotion.Ref) *promotion.Ref {
	if credentialID(&draft) == nil {
		return nil
	}
	return &ref
}

func publishedRef(record publications.ActivePublication, ref promotion.Ref, credential *promotion.Ref) *promotion.Ref {
	published := credentialID(record.Published)
	if published == nil {
		return nil
	}
	if drafted := credentialID(&record.Draft); credential != nil && drafted != nil && *drafted == *published {
		return credential
	}
	separate := ref + "/published"
	return &separate
}

func credentialID(draft *agentcontract.Draft) *string {
	if draft == nil || draft.Agent == nil {
		return nil
	}
	return draft.Agent.CredentialID
}

func exportDraft(draft agentcontract.Draft) agentcontract.Draft {
	return placed(publications.WithCredential(draft, nil), "")
}

func exportOptional(draft *agentcontract.Draft) *agentcontract.Draft {
	if draft == nil {
		return nil
	}
	exported := exportDraft(*draft)
	return &exported
}

func exportedDigest(draft agentcontract.Draft) string {
	return agentcontract.DraftDigest(agentcontract.NormalizeDraft(exportDraft(draft)))
}

func Render(publications []Publication) (*[]contract.ConfigurationAgentPublication, error) {
	if len(publications) == 0 {
		return nil, nil
	}
	rendered, err := promotion.TryMap(slices.SortedFunc(slices.Values(publications), byKey), func(publication Publication) (contract.ConfigurationAgentPublication, error) {
		return renderPublication(publication)
	})
	return &rendered, err
}

func renderPublication(publication Publication) (contract.ConfigurationAgentPublication, error) {
	draft, err := promotion.Convert[contract.ConfigurationAgentDraft](publication.Draft)
	if err != nil {
		return contract.ConfigurationAgentPublication{}, err
	}
	published, err := promotion.Convert[*contract.ConfigurationAgentDraft](publication.Published)
	if err != nil {
		return contract.ConfigurationAgentPublication{}, err
	}
	labels, err := promotion.Convert[*map[string]contract.ConfigurationAgentDraft](promotion.OptionalMap(publication.labels()))
	if err != nil {
		return contract.ConfigurationAgentPublication{}, err
	}
	credentials := publication.Kind.bearers()
	return contract.ConfigurationAgentPublication{
		Project:                publication.Project,
		Kind:                   contract.ConfigurationAgentPublicationKind(publication.Kind.name()),
		Draft:                  draft,
		Published:              published,
		Labels:                 labels,
		CredentialRef:          text(credentials.Credential),
		PublishedCredentialRef: text(credentials.PublishedCredential),
	}, nil
}

func text(ref *promotion.Ref) *string {
	if ref == nil {
		return nil
	}
	value := string(*ref)
	return &value
}
