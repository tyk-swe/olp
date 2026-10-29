package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	legacy "github.com/a2aproject/a2a-go/a2a"
	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/mcpgrants"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/workload"
)

const maxCardBytes = 1 << 20

type Runtime interface {
	consumerauth.Authenticator
	Release() *runtime.Release
	AuthorizeAgentResource(access.Authority, string, string, string, string) error
}

type AgentGrants interface {
	AgentBinding(access.Authority, string, agentcontract.Definition) (string, error)
	StartAgent(context.Context, access.Authority, string, agentcontract.Definition) (mcpgrants.Handoff, error)
	ResolveAgent(context.Context, access.Authority, string, agentcontract.Definition) ([]byte, string, error)
	ResolveAgentEntra(context.Context, access.Authority, string, agentcontract.Definition, workload.UserAssertion) ([]byte, string, error)
	ResolveAgentTask(context.Context, string, string, string, string, agentcontract.Definition) ([]byte, error)
}

type Server struct {
	Grants         AgentGrants
	Ledger         *Ledger
	Runtime        Runtime
	Egress         *egress.Policy
	Credentials    func(context.Context, string) ([]byte, error)
	TrustedProxies []netip.Prefix
	PublicOrigin   string
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
	if s.Grants == nil {
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
	handoff, err := s.Grants.StartAgent(r.Context(), authority, revision.PublicationID, *revision.Definition.Agent)
	if err != nil {
		http.Error(w, "Agent changed; start enrollment again", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(handoff)
}

func (s *Server) resolve(r *http.Request) (access.Authority, agentcontract.Revision, *runtime.Release, int) {
	if s.Runtime == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
	}
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	authority, failure := consumerauth.Authenticate(s.Runtime, r, token, "agents", s.TrustedProxies, time.Now())
	if failure != nil {
		return access.Authority{}, agentcontract.Revision{}, nil, failure.Status
	}
	if authority.EndUserDigest == "" {
		if authority.Policy.EndUserSource == nil {
			if authority.ProjectEndUserPolicy != nil || authority.Policy.EndUserPolicy != nil {
				return access.Authority{}, agentcontract.Revision{}, nil, http.StatusBadRequest
			}
		} else {
			identifier := ""
			if *authority.Policy.EndUserSource == "header" {
				if values := r.Header.Values("X-OLP-End-User"); len(values) == 1 {
					identifier = values[0]
				}
			}
			if !access.ValidEndUserIdentifier(identifier) {
				return access.Authority{}, agentcontract.Revision{}, nil, http.StatusBadRequest
			}
			digester, ok := s.Runtime.(interface{ EndUserDigest(*string, string) string })
			if !ok {
				return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
			}
			authority.EndUserDigest = digester.EndUserDigest(authority.ProjectID, identifier)
		}
	}
	if !authority.AllowsEndUser(authority.EndUserDigest) {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusForbidden
	}
	if authority.ProjectID == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
	}
	for _, revision := range release.Snapshot.AgentPublications {
		if revision.PublicationID != r.PathValue("agent") || revision.Kind != "agent" || revision.ProjectID != *authority.ProjectID || !revision.Current || revision.Definition.Agent == nil || !supportedProtocol(revision.Definition.Agent.Protocol) {
			continue
		}
		if err := s.Runtime.AuthorizeAgentResource(authority, "agents", revision.PublicationID, revision.ID, revision.ProjectID); err != nil {
			if errors.Is(err, runtime.ErrStaleAuthority) {
				return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
			}
			return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
		}
		return authority, revision, release, 0
	}
	return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
}

func (s *Server) getCard(w http.ResponseWriter, r *http.Request) {
	_, revision, _, status := s.resolve(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	original, err := s.loadCard(r.Context(), *revision.Definition.Agent, nil, false)
	if err != nil {
		http.Error(w, "Agent card unavailable", http.StatusBadGateway)
		return
	}
	response := protocol.AgentCard{
		Name: original.Name, Description: original.Description, Version: original.Version,
		SupportedInterfaces: []*protocol.AgentInterface{{URL: strings.TrimRight(s.PublicOrigin, "/") + "/a2a/" + revision.PublicationID, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: cardVersion(revision.Definition.Agent.Protocol)}},
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

func (s *Server) agentCredential(ctx context.Context, authority access.Authority, revision agentcontract.Revision) ([]byte, string, error) {
	if revision.Definition.Agent == nil || revision.Definition.Agent.OAuth == nil {
		return nil, "", nil
	}
	if s.Grants == nil {
		return nil, "", errors.New("agent grant unavailable")
	}
	if revision.Definition.Agent.OAuth.EntraOBO != nil {
		assertion, err := s.verifyAgentEntra(ctx, authority, *revision.Definition.Agent)
		if err != nil {
			return nil, "", errors.New("agent grant unavailable")
		}
		credential, binding, err := s.Grants.ResolveAgentEntra(ctx, authority, revision.PublicationID, *revision.Definition.Agent, assertion)
		if err != nil || len(credential) == 0 || binding == "" {
			return nil, "", errors.New("agent grant unavailable")
		}
		return credential, binding, nil
	}
	credential, binding, err := s.Grants.ResolveAgent(ctx, authority, revision.PublicationID, *revision.Definition.Agent)
	if err != nil || len(credential) == 0 || binding == "" {
		return nil, "", errors.New("agent grant unavailable")
	}
	return credential, binding, nil
}

func (s *Server) verifyAgentEntra(ctx context.Context, authority access.Authority, agent agentcontract.Definition) (workload.UserAssertion, error) {
	verifier, ok := s.Runtime.(interface {
		VerifyEntraUser(context.Context, string, access.Authority, string, string) (workload.UserAssertion, error)
	})
	if !ok {
		return workload.UserAssertion{}, errors.New("agent grant unavailable")
	}
	return verifier.VerifyEntraUser(ctx, workload.RequestAssertion(ctx), authority, agent.OAuth.EntraOBO.Issuer, agent.OAuth.ClientID)
}

func (s *Server) agentTaskCredential(ctx context.Context, authority access.Authority, revision agentcontract.Revision, binding string) ([]byte, error) {
	if revision.Definition.Agent == nil {
		return nil, errors.New("agent grant unavailable")
	}
	agent := *revision.Definition.Agent
	if agent.OAuth == nil && binding == "" {
		return nil, nil
	}
	if agent.OAuth == nil || binding == "" || s.Grants == nil {
		return nil, errors.New("agent grant unavailable")
	}
	if agent.OAuth.EntraOBO != nil {
		if _, err := s.verifyAgentEntra(ctx, authority, agent); err != nil {
			return nil, errors.New("agent grant unavailable")
		}
	}
	return s.Grants.ResolveAgentTask(ctx, binding, revision.PublicationID, revision.ProjectID, revision.ID, agent)
}

func (s *Server) loadCardWithCredential(ctx context.Context, definition agentcontract.Definition, oauthCredential []byte) (protocol.AgentCard, error) {
	return s.loadCard(ctx, definition, oauthCredential, true)
}

func (s *Server) loadCard(ctx context.Context, definition agentcontract.Definition, oauthCredential []byte, requireGrant bool) (protocol.AgentCard, error) {
	if requireGrant && definition.OAuth != nil && len(oauthCredential) == 0 {
		return protocol.AgentCard{}, errors.New("agent grant unavailable")
	}
	if s.Egress == nil {
		return protocol.AgentCard{}, errors.New("egress unavailable")
	}
	endpoint, err := s.Egress.ValidateEndpoint(definition.Endpoint)
	if err != nil {
		return protocol.AgentCard{}, err
	}
	cardURL := (&url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host, Path: "/.well-known/agent-card.json"}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return protocol.AgentCard{}, err
	}
	if len(oauthCredential) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(oauthCredential))
	} else if definition.OAuth == nil && definition.CredentialID != nil {
		if s.Credentials == nil {
			return protocol.AgentCard{}, errors.New("credential unavailable")
		}
		credential, err := s.Credentials(ctx, *definition.CredentialID)
		if err != nil || len(credential) == 0 {
			return protocol.AgentCard{}, errors.New("credential unavailable")
		}
		defer clear(credential)
		req.Header.Set("Authorization", "Bearer "+string(credential))
	}
	resp, err := s.Egress.Client(30 * time.Second).Do(req)
	if err != nil {
		return protocol.AgentCard{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.AgentCard{}, errors.New("card response")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCardBytes+1))
	if err != nil || len(body) > maxCardBytes {
		return protocol.AgentCard{}, errors.New("card size")
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != definition.CardDigest {
		return protocol.AgentCard{}, errors.New("card changed")
	}
	parsed, err := parseCard(definition.Protocol, body)
	if err != nil {
		return protocol.AgentCard{}, errors.New("invalid card")
	}
	for _, binding := range parsed.SupportedInterfaces {
		if binding != nil && binding.URL == definition.Endpoint && cardBindingMatches(definition.Protocol, binding) {
			return parsed, nil
		}
	}
	return protocol.AgentCard{}, errors.New("interface not declared")
}

func cardBindingMatches(version string, binding *protocol.AgentInterface) bool {
	if version == legacyProtocol {
		return (binding.ProtocolBinding == "" || binding.ProtocolBinding == protocol.TransportProtocolJSONRPC) && (binding.ProtocolVersion == a2av0.Version || binding.ProtocolVersion == protocol.ProtocolVersion(legacy.Version))
	}
	return binding.ProtocolBinding == protocol.TransportProtocolJSONRPC && binding.ProtocolVersion == protocol.Version
}

func (s *Server) ValidateCard(ctx context.Context, definition agentcontract.Definition) error {
	_, err := s.loadCard(ctx, definition, nil, false)
	return err
}

func parseCard(version string, body []byte) (protocol.AgentCard, error) {
	if version == legacyProtocol {
		var declared legacy.AgentCard
		if json.Unmarshal(body, &declared) != nil || declared.ProtocolVersion != string(a2av0.Version) && declared.ProtocolVersion != string(legacy.Version) {
			return protocol.AgentCard{}, errors.New("invalid card version")
		}
		return *a2av0.ToV1AgentCard(&declared), nil
	}
	var parsed protocol.AgentCard
	err := json.Unmarshal(body, &parsed)
	return parsed, err
}
