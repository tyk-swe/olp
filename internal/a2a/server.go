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

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
)

const maxCardBytes = 1 << 20

type Runtime interface {
	consumerauth.Authenticator
	Release() *runtime.Release
	AuthorizeAgentResource(access.Authority, string, string, string, string) error
}

type Server struct {
	Ledger         *Ledger
	Runtime        Runtime
	Egress         *egress.Policy
	Credentials    func(context.Context, string) ([]byte, error)
	TrustedProxies []netip.Prefix
	PublicOrigin   string
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /a2a/{agent}/.well-known/agent-card.json", s.getCard)
	mux.HandleFunc("POST /a2a/{agent}", s.rpc)
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
	if authority.ProjectID == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusNotFound
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		return access.Authority{}, agentcontract.Revision{}, nil, http.StatusServiceUnavailable
	}
	for _, revision := range release.Snapshot.AgentPublications {
		if revision.PublicationID != r.PathValue("agent") || revision.Kind != "agent" || revision.ProjectID != *authority.ProjectID || !revision.Current || revision.Definition.Agent == nil || revision.Definition.Agent.Protocol != "a2a-1.0.0" {
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
	original, err := s.loadCard(r.Context(), *revision.Definition.Agent)
	if err != nil {
		http.Error(w, "Agent card unavailable", http.StatusBadGateway)
		return
	}
	response := protocol.AgentCard{
		Name: original.Name, Description: original.Description, Version: original.Version,
		SupportedInterfaces: []*protocol.AgentInterface{protocol.NewAgentInterface(strings.TrimRight(s.PublicOrigin, "/")+"/a2a/"+revision.PublicationID, protocol.TransportProtocolJSONRPC)},
		Capabilities:        protocol.AgentCapabilities{Streaming: original.Capabilities.Streaming}, DefaultInputModes: original.DefaultInputModes, DefaultOutputModes: original.DefaultOutputModes,
		Skills:               publicSkills(original.Skills),
		SecuritySchemes:      protocol.NamedSecuritySchemes{"olp": protocol.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
		SecurityRequirements: protocol.SecurityRequirementsOptions{{"olp": {}}},
	}
	w.Header().Set("Content-Type", "application/a2a+json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
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

func (s *Server) loadCard(ctx context.Context, definition agentcontract.Definition) (protocol.AgentCard, error) {
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
	if definition.CredentialID != nil {
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
	var parsed protocol.AgentCard
	if json.Unmarshal(body, &parsed) != nil {
		return protocol.AgentCard{}, errors.New("invalid card")
	}
	for _, binding := range parsed.SupportedInterfaces {
		if binding != nil && binding.URL == definition.Endpoint && binding.ProtocolBinding == protocol.TransportProtocolJSONRPC && binding.ProtocolVersion == protocol.Version {
			return parsed, nil
		}
	}
	return protocol.AgentCard{}, errors.New("interface not declared")
}
