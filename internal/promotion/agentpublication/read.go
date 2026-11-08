package agentpublication

import (
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

const (
	publicationsQuery = `SELECT p.id::text,pr.name,p.kind,p.draft,v.definition FROM olp.agent_publications p JOIN olp.projects pr ON pr.id=p.project_id LEFT JOIN olp.agent_publication_revisions v ON v.id=p.latest_revision_id WHERE NOT p.retired`
	labelsQuery       = `SELECT l.publication_id::text,l.label,v.definition FROM olp.prompt_labels l JOIN olp.agent_publication_revisions v ON v.id=l.revision_id JOIN olp.agent_publications p ON p.id=l.publication_id WHERE NOT p.retired AND p.kind='prompt'`
)

type stored struct {
	ID        string
	Project   string
	Kind      string
	Draft     agentcontract.Draft
	Published *agentcontract.Draft
}

type storedLabel struct {
	Publication string
	Name        string
	Draft       agentcontract.Draft
}

func Read(ctx context.Context, q access.Queryer) ([]Publication, error) {
	records, err := promotion.Rows(ctx, q, pgx.RowToStructByPos[stored], publicationsQuery)
	if err != nil {
		return nil, err
	}
	labels, err := promotion.Rows(ctx, q, pgx.RowToStructByPos[storedLabel], labelsQuery)
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(func(yield func(Publication) bool) {
		for _, record := range records {
			if !yield(exported(record, labelsOf(labels, record.ID))) {
				return
			}
		}
	}, byKey), nil
}

func labelsOf(labels []storedLabel, publication string) map[string]agentcontract.Draft {
	return nonEmpty(maps.Collect(func(yield func(string, agentcontract.Draft) bool) {
		for _, label := range labels {
			if label.Publication == publication && !yield(label.Name, exportDraft(label.Draft)) {
				return
			}
		}
	}))
}

func exported(record stored, labels map[string]agentcontract.Draft) Publication {
	ref := derivedRef(keyOf(record.Project, record.Kind, record.Draft.Name))
	credential := credentialRef(record.Draft, ref)
	return Publication{
		Project:   record.Project,
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

func publishedRef(record stored, ref promotion.Ref, credential *promotion.Ref) *promotion.Ref {
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
	labels, err := promotion.Convert[*map[string]contract.ConfigurationAgentDraft](nonEmpty(publication.labels()))
	if err != nil {
		return contract.ConfigurationAgentPublication{}, err
	}
	agent, _ := publication.Kind.(Agent)
	return contract.ConfigurationAgentPublication{
		Project:                publication.Project,
		Kind:                   contract.ConfigurationAgentPublicationKind(publication.Kind.name()),
		Draft:                  draft,
		Published:              published,
		Labels:                 labels,
		CredentialRef:          text(agent.Credential),
		PublishedCredentialRef: text(agent.PublishedCredential),
	}, nil
}

func text(ref *promotion.Ref) *string {
	if ref == nil {
		return nil
	}
	value := string(*ref)
	return &value
}
