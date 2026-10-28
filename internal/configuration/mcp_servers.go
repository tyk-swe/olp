package configuration

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type MCPServerEntry struct {
	Delegation           *routing.MCPDelegation   `json:"delegation,omitempty"`
	Stdio                *routing.MCPStdioBinding `json:"stdio,omitempty"`
	OAuth                *routing.MCPOAuth        `json:"oauth,omitempty"`
	Name                 string                   `json:"name"`
	Project              string                   `json:"project"`
	Transport            string                   `json:"transport"`
	Endpoint             string                   `json:"endpoint"`
	Enabled              bool                     `json:"enabled"`
	CatalogDigest        string                   `json:"catalog_digest"`
	CredentialRef        *string                  `json:"credential_ref,omitempty"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices,omitempty"`
	CallsPerMinute       *int64                   `json:"calls_per_minute"`
	MaxConcurrency       *int64                   `json:"max_concurrency"`
	PerKeyCallsPerMinute *int64                   `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency *int64                   `json:"per_key_max_concurrency"`
}

func mcpEntryPrice(value *string) *string {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func mcpEntryLimit(value int64) *int64 {
	return &value
}

func normalizeMCPEntry(e *MCPServerEntry) {
	if e.ToolPrices == nil {
		e.ToolPrices = map[string]string{}
	}
	e.CallsPerMinute = mcpEntryLimit(routing.ToolLimitOr(e.CallsPerMinute, routing.DefaultToolCallsPerMinute))
	e.MaxConcurrency = mcpEntryLimit(routing.ToolLimitOr(e.MaxConcurrency, routing.DefaultToolMaxConcurrency))
	e.PerKeyCallsPerMinute = mcpEntryLimit(routing.ToolLimitOr(e.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute))
	e.PerKeyMaxConcurrency = mcpEntryLimit(routing.ToolLimitOr(e.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency))
}

func mcpDefinitionFromEntry(e MCPServerEntry, catalog *mcpservers.Catalog, current *mcpservers.Definition) *mcpservers.Definition {
	signingKeyID := func() *string {
		if current == nil || e.Delegation == nil {
			return nil
		}
		return current.DelegationKeyID
	}()
	prices := make(map[string]string, len(e.ToolPrices))
	for name, price := range e.ToolPrices {
		prices[name] = price
	}
	return &mcpservers.Definition{
		EntraClientSecretID: entraConfigurationSecret(e, current),
		DelegationKeyID:     signingKeyID, Delegation: e.Delegation.Clone(), OAuth: e.OAuth.Clone(), Stdio: e.Stdio.Clone(), Name: e.Name, Transport: e.Transport, Endpoint: e.Endpoint, Enabled: e.Enabled, Catalog: catalog,
		UnitPrice: mcpEntryPrice(e.UnitPrice), ToolPrices: prices,
		CallsPerMinute:       routing.ToolLimitOr(e.CallsPerMinute, routing.DefaultToolCallsPerMinute),
		MaxConcurrency:       routing.ToolLimitOr(e.MaxConcurrency, routing.DefaultToolMaxConcurrency),
		PerKeyCallsPerMinute: routing.ToolLimitOr(e.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
		PerKeyMaxConcurrency: routing.ToolLimitOr(e.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency),
	}
}

func mcpKey(e MCPServerEntry) [2]string {
	return [2]string{strings.ToLower(e.Project), strings.ToLower(e.Name)}
}
func mcpReference(e MCPServerEntry) string {
	encoded, _ := json.Marshal(mcpKey(e))
	sum := sha256.Sum256(encoded)
	return "mcp/" + hex.EncodeToString(sum[:])
}
func (s *Server) validateMCPServers(doc *Document) error {
	if len(doc.MCPServers) > 128 {
		return access.Invalid("mcp_servers", "Declare at most 128 MCP servers.")
	}
	seen := map[[2]string]bool{}
	for i := range doc.MCPServers {
		e := &doc.MCPServers[i]
		e.Name, e.Project = strings.TrimSpace(e.Name), strings.TrimSpace(e.Project)
		key := mcpKey(*e)
		digest, err := hex.DecodeString(e.CatalogDigest)
		if !guardrailLabel(e.Project) || seen[key] || !routing.MCPTransport(e.Transport).Valid() || err != nil || len(digest) != 32 || strings.ToLower(e.CatalogDigest) != e.CatalogDigest {
			return access.Invalid("mcp_servers", "Use unique project/name pairs, a supported MCP transport and a certified catalog digest.")
		}
		seen[key] = true
		in := mcpservers.Input{
			Delegation: e.Delegation.Clone(), OAuth: e.OAuth.Clone(), Stdio: e.Stdio.Clone(), Name: e.Name, Transport: e.Transport, Endpoint: e.Endpoint, Enabled: &e.Enabled,
			UnitPrice: e.UnitPrice, ToolPrices: e.ToolPrices,
			CallsPerMinute: e.CallsPerMinute, MaxConcurrency: e.MaxConcurrency,
			PerKeyCallsPerMinute: e.PerKeyCallsPerMinute, PerKeyMaxConcurrency: e.PerKeyMaxConcurrency,
		}
		if err = mcpservers.Validate(&in, s.Egress); err != nil {
			return err
		}
		e.Name, e.Endpoint = in.Name, in.Endpoint
		normalizeMCPEntry(e)
		if e.CredentialRef != nil && *e.CredentialRef != mcpReference(*e) {
			return access.Invalid("mcp_servers.credential_ref", "Use the deterministic exported MCP credential reference.")
		}
	}
	return nil
}
func exportMCPServers(ctx context.Context, q access.Queryer) ([]MCPServerEntry, error) {
	rows, err := q.Query(ctx, "SELECT s.name,p.name,s.transport,s.endpoint,s.enabled,v.digest,s.credential_id IS NOT NULL,v.unit_price::text,v.tool_prices,s.calls_per_minute,s.max_concurrency,s.per_key_calls_per_minute,s.per_key_max_concurrency,v.oauth,v.delegation,v.stdio FROM olp.mcp_servers s JOIN olp.projects p ON p.id=s.project_id JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id WHERE s.retired_at IS NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPServerEntry{}
	for rows.Next() {
		var e MCPServerEntry
		var credential bool
		var callsPerMinute, maxConcurrency, perKeyCallsPerMinute, perKeyMaxConcurrency int64
		if err = rows.Scan(&e.Name, &e.Project, &e.Transport, &e.Endpoint, &e.Enabled, &e.CatalogDigest, &credential, &e.UnitPrice, &e.ToolPrices, &callsPerMinute, &maxConcurrency, &perKeyCallsPerMinute, &perKeyMaxConcurrency, &e.OAuth, &e.Delegation, &e.Stdio); err != nil {
			return nil, err
		}
		e.CallsPerMinute = &callsPerMinute
		e.MaxConcurrency = &maxConcurrency
		e.PerKeyCallsPerMinute = &perKeyCallsPerMinute
		e.PerKeyMaxConcurrency = &perKeyMaxConcurrency
		if e.ToolPrices == nil {
			e.ToolPrices = map[string]string{}
		}
		if credential {
			ref := mcpReference(e)
			e.CredentialRef = &ref
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func findMCPServer(ctx context.Context, q access.Queryer, e MCPServerEntry) (*mcpservers.Definition, error) {
	var project string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", e.Project).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := mcpservers.FindActive(ctx, q, project, e.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}
func mcpEntry(d *mcpservers.Definition, project string) MCPServerEntry {
	prices := make(map[string]string, len(d.ToolPrices))
	for name, price := range d.ToolPrices {
		prices[name] = price
	}
	e := MCPServerEntry{
		Name: d.Name, Project: project, Transport: d.Transport, Endpoint: d.Endpoint, Enabled: d.Enabled, CatalogDigest: d.Catalog.Digest,
		Delegation: d.Delegation.Clone(), OAuth: d.OAuth.Clone(), Stdio: d.Stdio.Clone(), UnitPrice: mcpEntryPrice(d.UnitPrice), ToolPrices: prices,
		CallsPerMinute: mcpEntryLimit(d.CallsPerMinute), MaxConcurrency: mcpEntryLimit(d.MaxConcurrency),
		PerKeyCallsPerMinute: mcpEntryLimit(d.PerKeyCallsPerMinute), PerKeyMaxConcurrency: mcpEntryLimit(d.PerKeyMaxConcurrency),
	}
	if d.HasCredential {
		ref := mcpReference(e)
		e.CredentialRef = &ref
	}
	return e
}
func (s *Server) mcpMatches(ctx context.Context, q access.Queryer, e MCPServerEntry, d *mcpservers.Definition, bindings bindingSet) (bool, error) {
	if d == nil || !reflect.DeepEqual(mcpEntry(d, e.Project), e) {
		return false, nil
	}
	if e.CredentialRef != nil {
		if b, ok := bindings[*e.CredentialRef]; ok {
			current, err := s.Access.Keys.Read(ctx, q, s.Access.Installation, *d.CredentialID, secrets.MCPCredential)
			if err != nil {
				return false, err
			}
			defer clear(current)
			if subtle.ConstantTimeCompare(current, []byte(b.secret)) != 1 {
				return false, nil
			}
		}
	}
	return true, nil
}
func (s *Server) planMCPServers(ctx context.Context, q access.Queryer, doc *Document, bindings bindingSet, state *stateView, result *planResult) error {
	active := 0
	if len(doc.MCPServers) > 0 {
		if err := q.QueryRow(ctx, "SELECT count(*) FROM olp.mcp_servers WHERE retired_at IS NULL").Scan(&active); err != nil {
			return err
		}
	}
	for _, e := range doc.MCPServers {
		present := state.projects[strings.ToLower(e.Project)] != ""
		for _, p := range doc.Projects {
			present = present || strings.EqualFold(p.Name, e.Project)
		}
		if !present {
			result.conflict("mcp_server", e.Name, "The owning project is not declared or present.")
			continue
		}
		current, err := findMCPServer(ctx, q, e)
		if err != nil {
			return err
		}
		if e.OAuth != nil && e.OAuth.EntraOBO != nil && (current == nil || current.EntraClientSecretID == nil) {
			result.conflict("mcp_server", e.Name, "Install this server's Entra client secret locally before applying its metadata.")
		}
		if e.Delegation != nil && (current == nil || current.DelegationKeyID == nil) {
			result.conflict("mcp_server", e.Name, "Install this server's delegation signing key locally before applying its metadata.")
			continue
		}
		if e.CredentialRef != nil {
			if _, ok := bindings[*e.CredentialRef]; !ok && (current == nil || !current.HasCredential) {
				result.blocker("mcp_server", e.Name, "Supply the destination MCP bearer binding.")
			}
		}
		action := "create"
		if current != nil {
			action = "replace"
			equal, err := s.mcpMatches(ctx, q, e, current, bindings)
			if err != nil {
				return err
			}
			if equal {
				action = "reuse"
			}
		} else {
			active++
			if active > 128 {
				result.blocker("mcp_server", e.Name, "The destination would exceed 128 active MCP servers.")
			}
		}
		result.item("mcp_server", e.Name, action, "Recertify changed registrations against the reviewed catalog digest; history and credentials remain local.")
	}
	return nil
}

type preparedMCP struct {
	catalog    *mcpservers.Catalog
	observed   string
	reuse      bool
	limitsOnly bool
}

func (s *Server) prepareMCPServers(ctx context.Context, p access.Principal, doc *Document, bindings bindingSet) (map[[2]string]preparedMCP, error) {
	out := map[[2]string]preparedMCP{}
	if len(doc.MCPServers) == 0 {
		return out, nil
	}
	if err := s.validateMCPServers(doc); err != nil {
		return nil, err
	}
	if err := validateBindings(doc, bindings); err != nil {
		return nil, err
	}
	for _, e := range doc.MCPServers {
		d, err := findMCPServer(ctx, s.Access.Pool, e)
		if err != nil {
			return nil, err
		}
		if d != nil {
			if err = p.Project(&d.ProjectID, access.Change); err != nil {
				return nil, err
			}
		}
		equal, err := s.mcpMatches(ctx, s.Access.Pool, e, d, bindings)
		if err != nil {
			return nil, err
		}
		prepared := preparedMCP{reuse: equal}
		if d != nil {
			prepared.observed = d.ETag
		}
		if equal {
			out[mcpKey(e)] = prepared
			continue
		}
		if d != nil {
			probe := e
			current := mcpEntry(d, e.Project)
			probe.CallsPerMinute, probe.MaxConcurrency = current.CallsPerMinute, current.MaxConcurrency
			probe.PerKeyCallsPerMinute, probe.PerKeyMaxConcurrency = current.PerKeyCallsPerMinute, current.PerKeyMaxConcurrency
			sameExceptLimits, probeErr := s.mcpMatches(ctx, s.Access.Pool, probe, d, bindings)
			if probeErr != nil {
				return nil, probeErr
			}
			if sameExceptLimits {
				prepared.limitsOnly = true
				out[mcpKey(e)] = prepared
				continue
			}
		}
		var material []byte
		if e.CredentialRef != nil {
			if b, ok := bindings[*e.CredentialRef]; ok {
				material = []byte(b.secret)
			} else if d != nil && d.CredentialID != nil {
				if e.Endpoint != d.Endpoint {
					return nil, access.Invalid("secret_bindings", "Supply a new MCP bearer binding or remove credential_ref when changing the endpoint.")
				}
				material, err = s.Access.Keys.Read(ctx, s.Access.Pool, s.Access.Installation, *d.CredentialID, secrets.MCPCredential)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, access.Invalid("secret_bindings", "Supply the destination MCP bearer binding.")
			}
		}
		catalog, err := mcpservers.CertifyOptions(ctx, s.Egress, e.Endpoint, material, mcpservers.TransportOptions{Transport: routing.MCPTransport(e.Transport), Stdio: e.Stdio.Clone(), Executables: &mcpservers.Executables{Tier: s.Unconfined, Query: s.Access.Pool}})
		clear(material)
		if err != nil || catalog.Digest != e.CatalogDigest {
			return nil, access.Fail(422, "mcp_catalog_changed", "Certification must match the artifact's reviewed tool catalog digest.")
		}
		prepared.catalog = catalog
		out[mcpKey(e)] = prepared
	}
	return out, nil
}
func (s *Server) applyMCPServers(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, bindings bindingSet, prepared map[[2]string]preparedMCP) error {
	changed := false
	for _, e := range doc.MCPServers {
		var project string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", e.Project).Scan(&project); err != nil {
			return err
		}
		if err := p.Project(&project, access.Change); err != nil {
			return err
		}
		current, err := findMCPServer(ctx, tx, e)
		if err != nil {
			return err
		}
		cert, ok := prepared[mcpKey(e)]
		if !ok {
			return access.Invalid("mcp_servers", "Prepare certified registrations before applying.")
		}
		if current != nil && current.ETag != cert.observed || current == nil && cert.observed != "" {
			return access.Fail(412, "mcp_registration_changed", "The destination MCP registration changed during certification.")
		}
		if cert.reuse {
			continue
		}
		if cert.limitsOnly && current != nil {
			effective := routing.EffectiveToolLimits(
				routing.ToolLimitOr(e.CallsPerMinute, routing.DefaultToolCallsPerMinute),
				routing.ToolLimitOr(e.MaxConcurrency, routing.DefaultToolMaxConcurrency),
				routing.ToolLimitOr(e.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
				routing.ToolLimitOr(e.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency))
			if err = mcpservers.UpdateLimits(ctx, tx, current.ID, effective, access.NewID()); err != nil {
				return err
			}
			changed = true
			continue
		}
		next := mcpDefinitionFromEntry(e, cert.catalog, current)
		next.ProjectID = project
		if e.CredentialRef != nil {
			if b, ok := bindings[*e.CredentialRef]; ok {
				id := access.NewID()
				next.CredentialID = &id
				if err = s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, secrets.MCPCredential, []byte(b.secret), nil); err != nil {
					return err
				}
			} else if current != nil {
				next.CredentialID = current.CredentialID
			}
		}
		if err = mcpservers.Save(ctx, tx, p.UserID(), current, next); err != nil {
			return err
		}
		changed = true
		if current != nil && current.EntraClientSecretID != nil && next.EntraClientSecretID == nil {
			if _, keyErr := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.EntraClientSecretID, secrets.MCPEntraClient); keyErr != nil {
				return keyErr
			}
		}
		if current != nil && current.DelegationKeyID != nil && next.DelegationKeyID == nil {
			if _, keyErr := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.DelegationKeyID, secrets.MCPDelegationSigning); keyErr != nil {
				return keyErr
			}
		}
		if current != nil && current.CredentialID != nil && (next.CredentialID == nil || *current.CredentialID != *next.CredentialID) {
			if _, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.CredentialID, secrets.MCPCredential); err != nil {
				return err
			}
		}
	}
	if changed {
		_, err := access.AdvanceAuthority(ctx, tx)
		return err
	}
	return nil
}

func entraConfigurationSecret(e MCPServerEntry, current *mcpservers.Definition) *string {
	if current == nil || e.OAuth == nil || e.OAuth.EntraOBO == nil {
		return nil
	}
	return current.EntraClientSecretID
}
