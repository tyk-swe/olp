package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/workload"
)

type Runtime interface {
	consumerauth.Authenticator
	ReauthenticateDelegation(context.Context, string, string) (*access.Authority, error)
	Release() *release.Release
	AuthorizeAgentResource(access.Authority, string, string, string, string) error
}

type Enrollment interface {
	StartAgent(context.Context, access.Authority, string, agentcontract.Definition) (personal.Handoff, error)
}
type Options struct {
	Runtime        Runtime
	Executor       *agents.Executor
	Peers          *Peers
	Grants         Enrollment
	TrustedProxies []netip.Prefix
	PublicOrigin   string
}
type Server struct {
	runtime        Runtime
	executor       *agents.Executor
	peers          *Peers
	grants         Enrollment
	trustedProxies []netip.Prefix
	publicOrigin   string
}

func New(o Options) *Server {
	return &Server{runtime: o.Runtime, executor: o.Executor, peers: o.Peers, grants: o.Grants, trustedProxies: slices.Clone(o.TrustedProxies), publicOrigin: o.PublicOrigin}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /a2a/{agent}/.well-known/agent-card.json", s.getCard)
	mux.HandleFunc("POST /a2a/{agent}/grant-enrollments", s.startGrantEnrollment)
	mux.HandleFunc("POST /a2a/{agent}", s.withAssertion(s.rpc))
}

func (s *Server) withAssertion(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
		handler(w, r.WithContext(workload.WithAssertion(r.Context(), raw)))
	}
}

func (s *Server) startGrantEnrollment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	authority, revision, _, status := s.resolve(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if authority.WorkloadIssuerID == nil || authority.EndUserDigest == "" {
		http.Error(w, "A verified workload identity with an end user is required", http.StatusForbidden)
		return
	}
	if s.grants == nil {
		http.Error(w, "Grant enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	if revision.Definition.Agent.OAuth == nil {
		http.Error(w, "Agent OAuth is not configured", http.StatusBadRequest)
		return
	}
	if revision.Definition.Agent.OAuth.EntraOBO != nil {
		http.Error(w, "Entra authorization uses a verified workload assertion", http.StatusBadRequest)
		return
	}
	handoff, err := s.grants.StartAgent(r.Context(), authority, revision.PublicationID, *revision.Definition.Agent)
	if err != nil {
		http.Error(w, "Agent changed; start enrollment again", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(handoff)
}

func (s *Server) resolve(r *http.Request) (access.Authority, agentcontract.Revision, *release.Release, int) {
	if s.runtime == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
	}
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	authority, failure := consumerauth.Authenticate(s.runtime, r, token, "agents", s.trustedProxies, time.Now())
	if failure != nil {
		return access.Authority{}, agentcontract.Revision{}, nil, failure.Status
	}
	if authority.EndUserDigest == "" {
		if authority.Policy.EndUserSource == nil {
			if authority.ProjectEndUserPolicy != nil || authority.Policy.EndUserPolicy != nil {
				return access.Authority{}, agentcontract.Revision{}, nil, http.StatusBadRequest
			}
		} else {
			digest, failure := consumerauth.HeaderEndUserDigest(s.runtime, r, authority)
			if failure != nil {
				return access.Authority{}, agentcontract.Revision{}, nil, failure.Status
			}
			authority.EndUserDigest = digest
		}
	}
	if !authority.AllowsEndUser(authority.EndUserDigest) {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusForbidden
	}
	if authority.ProjectID == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
	}
	current := s.runtime.Release()
	if current == nil || current.Snapshot == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
	}
	for _, revision := range current.Snapshot.AgentPublications {
		if revision.PublicationID != r.PathValue("agent") || revision.Kind != "agent" || revision.ProjectID != *authority.ProjectID || !revision.Current || revision.Definition.Agent == nil || !supportedProtocol(revision.Definition.Agent.Protocol) {
			continue
		}
		if err := s.runtime.AuthorizeAgentResource(authority, "agents", revision.PublicationID, revision.ID, revision.ProjectID); err != nil {
			if errors.Is(err, release.ErrStaleAuthority) {
				return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
			}
			return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
		}
		return authority, revision, current, 0
	}
	return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
}

func (s *Server) getCard(w http.ResponseWriter, r *http.Request) {
	_, revision, _, status := s.resolve(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	original, err := s.peers.Card(r.Context(), *revision.Definition.Agent)
	if err != nil {
		http.Error(w, "Agent card unavailable", http.StatusBadGateway)
		return
	}
	response := protocol.AgentCard{
		Name: original.Name, Description: original.Description, Version: original.Version,
		SupportedInterfaces: []*protocol.AgentInterface{{URL: strings.TrimRight(s.publicOrigin, "/") + "/a2a/" + revision.PublicationID, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: cardVersion(revision.Definition.Agent.Protocol)}},
		Capabilities:        protocol.AgentCapabilities{Streaming: original.Capabilities.Streaming}, DefaultInputModes: original.DefaultInputModes, DefaultOutputModes: original.DefaultOutputModes,
		Skills:               publicSkills(original.Skills),
		SecuritySchemes:      protocol.NamedSecuritySchemes{"olp": protocol.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
		SecurityRequirements: protocol.SecurityRequirementsOptions{{"olp": {}}},
	}
	w.Header().Set("Content-Type", "application/a2a+json")
	w.Header().Set("Cache-Control", "no-store")
	if revision.Definition.Agent.Protocol == legacyProtocol {
		producer := a2av0.NewStaticAgentCardProducer(&response)
		data, err := producer.(a2asrv.AgentCardJSONProducer).CardJSON(r.Context())
		if err != nil {
			http.Error(w, "Agent card unavailable", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(data)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

func cardVersion(version string) protocol.ProtocolVersion {
	if version == legacyProtocol {
		return a2av0.Version
	}
	return protocol.Version
}

func publicSkills(skills []protocol.AgentSkill) []protocol.AgentSkill {
	return slices.Collect(func(yield func(protocol.AgentSkill) bool) {
		for _, skill := range skills {
			if !yield(protocol.AgentSkill{ID: skill.ID, Name: skill.Name, Description: skill.Description, Tags: skill.Tags, Examples: skill.Examples, InputModes: skill.InputModes, OutputModes: skill.OutputModes}) {
				return
			}
		}
	})
}

func (s *Server) streamAuthorized(r *http.Request, authority access.Authority, revision agentcontract.Revision) bool {
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	current, failure := consumerauth.Authenticate(streamAuthenticator{s.runtime}, r, token, "agents", s.trustedProxies, time.Now())
	return failure == nil && current.ID == authority.ID && current.DelegationAgentID == authority.DelegationAgentID && current.DelegationPairDigest == authority.DelegationPairDigest && current.DelegationTokenDigest == authority.DelegationTokenDigest && s.runtime.AuthorizeAgentResource(current, "agents", revision.PublicationID, revision.ID, revision.ProjectID) == nil
}

type streamAuthenticator struct{ Runtime }

func (a streamAuthenticator) AuthenticateDelegation(ctx context.Context, token, assertion string) (*access.Authority, error) {
	return a.Runtime.ReauthenticateDelegation(ctx, token, assertion)
}
