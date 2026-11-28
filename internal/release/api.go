package release

import (
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type API struct {
	server  *access.Server
	manager *Manager
}

func NewAPI(server *access.Server, manager *Manager) *API {
	return &API{server: server, manager: manager}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/agent-delegations", a.listDelegations)
	a.server.Route(mux, "POST /api/v1/delegation-consents", a.createConsent)
	a.server.Route(mux, "GET /api/v1/delegation-options", a.delegationOptions)
	a.server.Route(mux, "GET /api/v1/runtime-generations", a.listGenerations)
}

type delegationConsentRequest struct {
	AgentID         string   `json:"agent_id"`
	APIKeyID        string   `json:"api_key_id"`
	Scopes          []string `json:"scopes,omitempty"`
	AllowedRoutes   []string `json:"allowed_routes"`
	AllowedToolsets []string `json:"allowed_toolsets,omitempty"`
	AllowedAgents   []string `json:"allowed_agents,omitempty"`
	TTLSeconds      int64    `json:"ttl_seconds,omitempty"`
}

func (a *API) listDelegations(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, pageErr := httpkit.Page(r)
	if pageErr != nil {
		return httpkit.Reply{}, pageErr
	}
	project, projectErr := p.ProjectFilter(r)
	if projectErr != nil {
		return httpkit.Reply{}, projectErr
	}
	delegation := r.URL.Query().Get("delegation_id")
	if delegation != "" && !ids.Canonical(delegation) {
		return httpkit.Reply{}, problem.Invalid("delegation_id", "Use a canonical delegation UUID.")
	}
	records, recordsErr := delegationRecords(r.Context(), a.server.Pool, a.server.Auth, delegationFilter{principal: p, page: page, project: project, delegation: delegation})
	if recordsErr != nil {
		return httpkit.Reply{}, recordsErr
	}
	views, viewsErr := delegationViews(r.Context(), a.server.Pool, records, page.Limit)
	if viewsErr != nil {
		return httpkit.Reply{}, viewsErr
	}
	return httpkit.ListReplyBy(views, page, func(item DelegationView) string { return item.ID }), nil
}

func (a *API) delegationOptions(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	keyID, err := ids.Parse(r.URL.Query().Get("api_key_id"))
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	return access.Transact(a.server, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		projectID, projectErr := delegatingKeyProject(ctx, tx, keyID)
		if projectErr != nil {
			return httpkit.Reply{}, access.Event{}, projectErr
		}
		if projectID == nil {
			return httpkit.Reply{}, access.Event{}, problem.Forbidden()
		}
		if err := p.Project(projectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		resources, resourcesErr := loadDelegatableResources(ctx, tx, *projectID)
		if resourcesErr != nil {
			return httpkit.Reply{}, access.Event{}, resourcesErr
		}
		authorityID, authorityErr := installationAuthority(ctx, tx)
		if authorityErr != nil {
			return httpkit.Reply{}, access.Event{}, authorityErr
		}
		options, optionsErr := a.manager.memberConsentOptions(authorityID, *projectID, keyID, resources)
		if optionsErr != nil {
			return httpkit.Reply{}, access.Event{}, optionsErr
		}
		return httpkit.OK(struct {
			Agents []delegationOption `json:"agents"`
		}{Agents: options}), access.Event{}, nil
	})
}

func (a *API) createConsent(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input delegationConsentRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	keyID, keyErr := ids.Parse(input.APIKeyID)
	if keyErr != nil {
		return httpkit.Reply{}, keyErr
	}
	agentID, agentErr := ids.Parse(input.AgentID)
	if agentErr != nil {
		return httpkit.Reply{}, agentErr
	}
	ctx := r.Context()
	return access.Transact(a.server, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := a.server.ConsumeRecent(r, tx, p, "agent_delegation", keyID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		bounds := DelegationBounds{Scopes: consentScopes(input.Scopes), AllowedRoutes: input.AllowedRoutes, AllowedToolsets: input.AllowedToolsets, AllowedAgents: input.AllowedAgents, TTLSeconds: input.TTLSeconds}
		if !validDelegationBounds(bounds) {
			return httpkit.Reply{}, access.Event{}, problem.Invalid("scopes", "Choose valid scopes and resources and a duration of at most 15 minutes.")
		}
		authorityID, authorityErr := installationAuthority(ctx, tx)
		if authorityErr != nil {
			return httpkit.Reply{}, access.Event{}, authorityErr
		}
		target, targetErr := a.manager.memberConsentTarget(authorityID, agentID, keyID)
		if targetErr != nil {
			return httpkit.Reply{}, access.Event{}, targetErr
		}
		if err := p.Project(target.key.ProjectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if target.key.ProjectID == nil || target.agent.ProjectID == nil || *target.key.ProjectID != *target.agent.ProjectID || !slices.Contains(target.key.Policy.Scopes, "delegate") || !delegationResourcesAllowed(target.agent, target.key, bounds.AllowedRoutes, bounds.AllowedToolsets, bounds.AllowedAgents, time.Now()) {
			return httpkit.Reply{}, access.Event{}, problem.Forbidden()
		}
		code := "olpc_" + secrets.Token()
		expires := boundDelegationExpiry(time.Now().Add(delegationTTL(bounds.TTLSeconds)), target.key.ExpiresAt)
		if err := insertConsent(ctx, tx, pendingConsent{
			tokenDigest:    a.manager.auth.Digest(secrets.DelegationTokenDigest, "unissued:"+code),
			consentDigest:  a.manager.auth.Digest(secrets.DelegationConsentDigest, code),
			memberID:       p.ID,
			projectID:      *target.key.ProjectID,
			keyID:          keyID,
			agentID:        agentID,
			issuerRevision: target.revision,
			bounds:         bounds,
			expiresAt:      expires,
		}); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		pair, _ := a.manager.delegationPair(*target.key.ProjectID, keyID, agentID, &p.ID)
		return httpkit.Reply{Status: http.StatusCreated, Body: map[string]any{"consent": code, "expires_at": expires, "delegation_pair_digest": pair}}, access.Event{Action: "agent_delegation.consent", Resource: "api_key", ID: keyID}, nil
	})
}

func (a *API) listGenerations(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	page, pageErr := httpkit.Page(r)
	if pageErr != nil {
		return httpkit.Reply{}, pageErr
	}
	items, itemsErr := runtimeGenerations(r.Context(), a.server.Pool, page)
	if itemsErr != nil {
		return httpkit.Reply{}, itemsErr
	}
	return httpkit.ListReply(items, page), nil
}
