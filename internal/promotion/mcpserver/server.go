package mcpserver

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	kind       = contract.ConfigurationPlanItemKindMcpServer
	maxServers = 128
)

type Server struct {
	Project       string
	Name          string
	Transport     Transport
	Enabled       bool
	CatalogDigest string
	UnitPrice     *string
	ToolPrices    map[string]string
	Limits        routing.ToolLimits
}

type Transport interface{ transport() }

type Stdio struct{ Binding routing.MCPStdioBinding }

type HTTP struct {
	Endpoint   string
	SSE        bool
	OAuth      *routing.MCPOAuth
	Delegation *routing.MCPDelegation
	Bearer     *promotion.Ref
}

func (Stdio) transport() {}
func (HTTP) transport()  {}

type key [2]string

func Parse(document contract.ConfigurationDocument, policy *egress.Policy) ([]Server, error) {
	entries := promotion.ValueOf(document.McpServers)
	if len(entries) > maxServers {
		return nil, access.Invalid("mcp_servers", "Declare at most 128 MCP servers.")
	}
	servers, err := promotion.TryMapIndex(entries, func(index int, entry contract.ConfigurationMCPServer) (Server, error) {
		return parseEntry(entry, entries[:index], policy)
	})
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(slices.Values(servers), byKey), nil
}

func parseEntry(entry contract.ConfigurationMCPServer, prior []contract.ConfigurationMCPServer, policy *egress.Policy) (Server, error) {
	identity := keyOf(entry.Project, entry.Name)
	if !promotion.ValidLabel(strings.TrimSpace(entry.Project)) ||
		slices.ContainsFunc(prior, func(other contract.ConfigurationMCPServer) bool { return keyOf(other.Project, other.Name) == identity }) ||
		!routing.MCPTransport(entry.Transport).Valid() || !certifiedDigest(entry.CatalogDigest) {
		return Server{}, access.Invalid("mcp_servers", "Use unique project/name pairs, a supported MCP transport and a certified catalog digest.")
	}
	prepared, err := registration.Prepare(registration.Input{
		Delegation: delegationOf(entry.Delegation), OAuth: oauthOf(entry.Oauth), Stdio: stdioOf(entry.Stdio),
		Name: strings.TrimSpace(entry.Name), Transport: string(entry.Transport), Endpoint: promotion.ValueOf(entry.Endpoint), Enabled: &entry.Enabled,
		UnitPrice: promotion.Pointer(entry.UnitPrice), ToolPrices: promotion.ValueOf(entry.ToolPrices),
		CallsPerMinute: widen(entry.CallsPerMinute), MaxConcurrency: widen(entry.MaxConcurrency),
		PerKeyCallsPerMinute: widen(entry.PerKeyCallsPerMinute), PerKeyMaxConcurrency: widen(entry.PerKeyMaxConcurrency),
	}, policy)
	if err != nil {
		return Server{}, err
	}
	if prepared.Stdio != nil && entry.CredentialRef != nil {
		return Server{}, access.Invalid("stdio", "Pin an installed executable and digest without HTTP credentials or endpoint.")
	}
	ref := refOf(identity)
	if entry.CredentialRef != nil && promotion.Ref(*entry.CredentialRef) != ref {
		return Server{}, access.Invalid("mcp_servers.credential_ref", "Use the deterministic exported MCP credential reference.")
	}
	return Server{
		Project: strings.TrimSpace(entry.Project), Name: prepared.Name, Enabled: entry.Enabled, CatalogDigest: entry.CatalogDigest,
		Transport: transportOf(wire{
			transport: routing.MCPTransport(prepared.Transport), endpoint: prepared.Endpoint, stdio: prepared.Stdio,
			oauth: prepared.OAuth, delegation: prepared.Delegation, bearer: bearerOf(entry.CredentialRef != nil, ref),
		}),
		UnitPrice: prepared.UnitPrice, ToolPrices: nonNil(prepared.ToolPrices),
		Limits: routing.ToolLimits{
			CallsPerMinute:       routing.ToolLimitOr(prepared.CallsPerMinute, routing.DefaultToolCallsPerMinute),
			MaxConcurrency:       routing.ToolLimitOr(prepared.MaxConcurrency, routing.DefaultToolMaxConcurrency),
			PerKeyCallsPerMinute: routing.ToolLimitOr(prepared.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
			PerKeyMaxConcurrency: routing.ToolLimitOr(prepared.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency),
		},
	}, nil
}

func certifiedDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(digest) == digest
}

func keyOf(project, name string) key {
	return key{strings.ToLower(strings.TrimSpace(project)), strings.ToLower(strings.TrimSpace(name))}
}

func (s Server) key() key { return keyOf(s.Project, s.Name) }

func byKey(a, b Server) int {
	return cmp.Or(cmp.Compare(a.key()[0], b.key()[0]), cmp.Compare(a.key()[1], b.key()[1]))
}

func refOf(identity key) promotion.Ref {
	sum := sha256.Sum256([]byte(promotion.TupleKey(identity[:]...)))
	return promotion.Ref("mcp/" + hex.EncodeToString(sum[:]))
}

func bearerOf(bound bool, ref promotion.Ref) *promotion.Ref {
	if !bound {
		return nil
	}
	return &ref
}
