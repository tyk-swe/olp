package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	legacy "github.com/a2aproject/a2a-go/a2a"
	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

const maxCardBytes = 1 << 20

func (s *Peers) loadCard(ctx context.Context, definition agentcontract.Definition, credential []byte) (protocol.AgentCard, error) {
	if s.egress == nil {
		return protocol.AgentCard{}, errors.New("egress unavailable")
	}
	endpoint, err := s.egress.ValidateEndpoint(definition.Endpoint)
	if err != nil {
		return protocol.AgentCard{}, err
	}
	cardURL := (&url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host, Path: "/.well-known/agent-card.json"}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return protocol.AgentCard{}, err
	}
	if len(credential) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(credential))
	}
	resp, err := s.egress.Client(30 * time.Second).Do(req)
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

func (s *Peers) ValidateCard(ctx context.Context, definition agentcontract.Definition) error {
	_, err := s.Card(ctx, definition)
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
