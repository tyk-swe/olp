package publishedprompt

import (
	"errors"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/agentcontract"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/prompts"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type Scope struct {
	Snapshot     *routing.View
	MaxBodyBytes int64
	Admit        func(*routing.Route) *fault.Error
}

func Referenced(doc oif.Document, family wire.Format) bool {
	_, found := referenceIn(doc, family)
	return found
}

func inputLimit(routeLimit *int64, installationLimit int64) int64 {
	if routeLimit == nil {
		return installationLimit
	}
	return min(*routeLimit, installationLimit)
}

func Expand(scope Scope, family wire.Format, doc oif.Document, pathModel string) (oif.Document, *usage.PromptReference, *fault.Error) {
	ref, found := referenceIn(doc, family)
	if !found {
		return doc, nil, nil
	}
	chosen, e := ref.selection(doc, family)
	if e != nil {
		return oif.Document{}, nil, e
	}
	route, e := scope.route(doc, family, pathModel, ref)
	if e != nil {
		return oif.Document{}, nil, e
	}
	revision, found := scope.revision(route, publicationID(family, chosen.ID), chosen)
	if !found {
		return oif.Document{}, nil, ref.invalid("The published prompt revision was not found for this project.")
	}
	rendered, err := revision.Definition.Prompt.RenderMessages(chosen.Variables, inputLimit(route.MaxBodyBytes, scope.MaxBodyBytes))
	if errors.Is(err, prompts.ErrRenderTooLarge) {
		return oif.Document{}, nil, ingress.BodyTooLarge()
	}
	if err != nil {
		return oif.Document{}, nil, ref.invalid("Prompt variables do not match the published definition.")
	}
	expanded, err := ref.replace(doc, family, rendered, revision.Definition.Prompt.DefaultParameters)
	if err != nil {
		return oif.Document{}, nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
	}
	if int64(expanded.Len()) > scope.MaxBodyBytes {
		return oif.Document{}, nil, ingress.BodyTooLarge()
	}
	return expanded, &usage.PromptReference{ID: revision.PublicationID, RevisionID: revision.ID}, nil
}

func (scope Scope) route(doc oif.Document, family wire.Format, pathModel string, ref reference) (routing.Route, *fault.Error) {
	slug, e := routeSlug(doc, family, pathModel)
	if e != nil {
		return routing.Route{}, e
	}
	if scope.Snapshot == nil {
		return routing.Route{}, fault.ServerError(http.StatusServiceUnavailable, "runtime_unavailable", "No runtime release is available.")
	}
	route, found := scope.Snapshot.FindRoute(slug)
	if !found {
		return routing.Route{}, fault.ModelNotFound(slug)
	}
	if e := scope.Admit(&route); e != nil {
		return routing.Route{}, e
	}
	if route.Fidelity.Strict() {
		return routing.Route{}, ref.invalid("Published prompts require a transformed route.")
	}
	return route, nil
}

func (scope Scope) revision(route routing.Route, publication string, chosen selection) (agentcontract.Revision, bool) {
	for _, revision := range scope.Snapshot.AgentPublications() {
		if revision.Kind != "prompt" || revision.PublicationID != publication || route.ProjectID == nil || revision.ProjectID != *route.ProjectID || revision.Definition.Prompt == nil {
			continue
		}
		if chosen.RevisionID != "" && revision.ID != chosen.RevisionID || chosen.Label != "" && !slices.Contains(revision.Labels, chosen.Label) {
			continue
		}
		return revision, true
	}
	return agentcontract.Revision{}, false
}

func (ref reference) replace(doc oif.Document, family wire.Format, rendered []prompts.Message, parameters *prompts.Parameters) (oif.Document, error) {
	defaulted, err := applyDefaults(doc, family, parameters)
	if err != nil {
		return oif.Document{}, err
	}
	conversation, system, err := liftSystem(family, rendered)
	if err != nil {
		return oif.Document{}, err
	}
	messages, err := encodeMessages(family, conversation)
	if err != nil {
		return oif.Document{}, err
	}
	return oif.Apply(defaulted, slices.Concat([]oif.Change{
		{Pointer: "/" + ref.param, Remove: true, Origin: oif.ResourceBinding, Reason: "prompt reference consumed"},
		{Pointer: "/" + ref.input, Value: string(messages), Origin: oif.ResourceBinding, Reason: "published prompt rendered"},
	}, system))
}
