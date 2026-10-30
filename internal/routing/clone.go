package routing

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
)

// Clone owns all published configuration values. Derived contracts are omitted
// and must be compiled by the new owner before serving.
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	out := &Snapshot{Commitments: maps.Clone(s.Commitments), Generation: s.Generation, Providers: maps.Clone(s.Providers), Routes: maps.Clone(s.Routes), CodeRoutes: maps.Clone(s.CodeRoutes), CodeConnections: maps.Clone(s.CodeConnections), InstallationPolicy: s.InstallationPolicy.Clone(), KeyPolicies: maps.Clone(s.KeyPolicies)}
	out.MCPServers = slices.Collect(func(yield func(MCPServer) bool) {
		for _, server := range s.MCPServers {
			if !yield(MCPServer{
				ID: server.ID, ProjectID: server.ProjectID, RevisionID: server.RevisionID, Endpoint: server.Endpoint,
				Transport: server.Transport, Protocol: server.Protocol, Digest: server.Digest,
				CredentialID: copyPointer(server.CredentialID), UnitPrice: copyPointer(server.UnitPrice), ToolPrices: maps.Clone(server.ToolPrices),
				OAuth: server.OAuth.Clone(), Delegation: server.Delegation.Clone(), Stdio: server.Stdio.Clone(),
				CallsPerMinute: server.CallsPerMinute, MaxConcurrency: server.MaxConcurrency,
				PerKeyCallsPerMinute: server.PerKeyCallsPerMinute, PerKeyMaxConcurrency: server.PerKeyMaxConcurrency,
				Tools: slices.Collect(func(yieldTool func(MCPTool) bool) {
					for _, tool := range server.Tools {
						if !yieldTool(MCPTool{Name: tool.Name, Description: tool.Description, InputSchema: slices.Clone(tool.InputSchema)}) {
							return
						}
					}
				}),
			}) {
				return
			}
		}
	})
	out.AgentPublications = slices.Clone(s.AgentPublications)
	for i, v := range out.AgentPublications {
		out.AgentPublications[i] = agentcontract.CloneRevision(v)
	}
	for id, values := range out.Commitments {
		values = slices.Clone(values)
		for i := range values {
			values[i].EffectiveAt = copyPointer(values[i].EffectiveAt)
			values[i].EndsAt = copyPointer(values[i].EndsAt)
		}
		out.Commitments[id] = values
	}
	for id, p := range out.Providers {
		out.Providers[id] = cloneProvider(p)
	}
	for id, r := range out.Routes {
		out.Routes[id] = cloneRoute(r)
	}
	for id, p := range out.KeyPolicies {
		out.KeyPolicies[id] = p.Clone()
	}
	for id, r := range out.CodeRoutes {
		out.CodeRoutes[id] = cloneCodeRoute(r)
	}
	for id, c := range out.CodeConnections {
		out.CodeConnections[id] = cloneConfiguration(c)
	}
	return out
}

func copyPointer[T any](v *T) *T {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
func cloneValues(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(m)
	for k, v := range out {
		out[k] = bytes.Clone(v)
	}
	return out
}

func (p *Policy) Clone() *Policy {
	if p == nil {
		return nil
	}
	out := *p
	out.AllowedStrategies = slices.Clone(p.AllowedStrategies)
	out.Constraints = clonePreferences(p.Constraints)
	out.Defaults = clonePreferences(p.Defaults)
	return &out
}
func clonePreferences(p Preferences) Preferences {
	p.Strategy = copyPointer(p.Strategy)
	p.AllowFallbacks = copyPointer(p.AllowFallbacks)
	p.Order = slices.Clone(p.Order)
	p.Ignore = slices.Clone(p.Ignore)
	p.Only = slices.Clone(p.Only)
	p.Quantizations = slices.Clone(p.Quantizations)
	p.Regions = slices.Clone(p.Regions)
	p.PreferredMaxLatencyMS = copyPointer(p.PreferredMaxLatencyMS)
	p.PreferredMinThroughput = copyPointer(p.PreferredMinThroughput)
	p.DenyDataCollection = copyPointer(p.DenyDataCollection)
	p.MaxPrice = bytes.Clone(p.MaxPrice)
	p.MaxCost = copyPointer(p.MaxCost)
	p.RequireParameters = copyPointer(p.RequireParameters)
	p.RequireZeroDataRetention = copyPointer(p.RequireZeroDataRetention)
	p.MaxAttempts = copyPointer(p.MaxAttempts)
	p.Priority = copyPointer(p.Priority)
	return p
}
func cloneSupply(s Supply) Supply {
	s.DailyCostLimit = copyPointer(s.DailyCostLimit)
	s.MonthlyCostLimit = copyPointer(s.MonthlyCostLimit)
	s.PriorityShares = copyPointer(s.PriorityShares)
	s.SaturationPercent = copyPointer(s.SaturationPercent)
	return s
}
func cloneLimits(l *Limits) *Limits {
	if l == nil {
		return nil
	}
	out := *l
	out.RequestsPerMinute = copyPointer(l.RequestsPerMinute)
	out.TokensPerMinute = copyPointer(l.TokensPerMinute)
	out.MaxConcurrency = copyPointer(l.MaxConcurrency)
	out.Supply = cloneSupply(l.Supply)
	return &out
}
func cloneSlot(s Slot) Slot {
	s.CredentialID = copyPointer(s.CredentialID)
	s.CredentialVersion = copyPointer(s.CredentialVersion)
	s.AllowedModels = slices.Clone(s.AllowedModels)
	s.AllowedRoutes = slices.Clone(s.AllowedRoutes)
	s.AllowedAPIKeys = slices.Clone(s.AllowedAPIKeys)
	s.RequestsPerMinute = copyPointer(s.RequestsPerMinute)
	s.TokensPerMinute = copyPointer(s.TokensPerMinute)
	s.MaxConcurrency = copyPointer(s.MaxConcurrency)
	s.Supply = cloneSupply(s.Supply)
	return s
}
func cloneProvider(p Provider) Provider {
	// Config.Clone owns nested defaults, bindings, model documents and networking.
	c := p.Connector().Clone()
	p.Network = c.Network
	p.BatchStorage = c.BatchStorage
	p.TuningDatasetURIPrefixes = c.TuningDatasetURIPrefixes
	p.DocumentURIPrefixes = c.DocumentURIPrefixes
	p.SemanticHeaders = c.SemanticHeaders
	p.QuerySettings = c.QuerySettings
	p.OperationDefaults = c.OperationDefaults
	p.Bindings = c.Bindings
	p.PluginOptions = c.PluginOptions
	p.CredentialHeaders = c.CredentialHeaders
	p.Models = c.Models
	p.ActiveCredential = copyPointer(p.ActiveCredential)
	p.Capabilities = slices.Clone(p.Capabilities)
	p.ParameterDefaults = cloneValues(p.ParameterDefaults)
	p.ProjectID = copyPointer(p.ProjectID)
	p.Limits = cloneLimits(p.Limits)
	p.Commitment = copyPointer(p.Commitment)
	if p.Commitment != nil {
		p.Commitment.EffectiveAt = copyPointer(p.Commitment.EffectiveAt)
		p.Commitment.EndsAt = copyPointer(p.Commitment.EndsAt)
	}
	p.PendingCommitment = copyPointer(p.PendingCommitment)
	if p.PendingCommitment != nil {
		p.PendingCommitment.EffectiveAt = copyPointer(p.PendingCommitment.EffectiveAt)
		p.PendingCommitment.EndsAt = copyPointer(p.PendingCommitment.EndsAt)
	}
	p.HealthProbe = copyPointer(p.HealthProbe)
	p.Slots = slices.Clone(p.Slots)
	for i := range p.Slots {
		p.Slots[i] = cloneSlot(p.Slots[i])
	}
	return p
}
func cloneRoute(r Route) Route {
	r.compiledContentPolicy = nil
	r.Operations = slices.Clone(r.Operations)
	r.Targets = slices.Clone(r.Targets)
	for i := range r.Targets {
		r.Targets[i].Tags = slices.Clone(r.Targets[i].Tags)
		r.Targets[i].Shadow = copyPointer(r.Targets[i].Shadow)
	}
	r.Policy = r.Policy.Clone()
	r.ProjectID = copyPointer(r.ProjectID)
	r.ContentPolicy = r.ContentPolicy.Clone()
	r.Behavior = cloneBehavior(r.Behavior)
	return r
}
func cloneBehavior(b Behavior) Behavior {
	b.Cache = copyPointer(b.Cache)
	if b.Cache != nil {
		b.Cache.Operations = slices.Clone(b.Cache.Operations)
		b.Cache.CountHitsAgainst = slices.Clone(b.Cache.CountHitsAgainst)
	}
	b.PromptCache = copyPointer(b.PromptCache)
	if b.PromptCache != nil {
		b.PromptCache.Positions = slices.Clone(b.PromptCache.Positions)
	}
	b.MaxBodyBytes = copyPointer(b.MaxBodyBytes)
	b.Fallbacks = slices.Clone(b.Fallbacks)
	for i := range b.Fallbacks {
		b.Fallbacks[i].On = slices.Clone(b.Fallbacks[i].On)
	}
	b.Selectors = slices.Clone(b.Selectors)
	for i := range b.Selectors {
		b.Selectors[i].Tags = slices.Clone(b.Selectors[i].Tags)
		b.Selectors[i].When = clonePredicate(b.Selectors[i].When)
	}
	b.Retry = maps.Clone(b.Retry)
	b.Affinity = copyPointer(b.Affinity)
	b.Passthrough = copyPointer(b.Passthrough)
	if b.Passthrough != nil {
		b.Passthrough.Methods = slices.Clone(b.Passthrough.Methods)
		b.Passthrough.Paths = slices.Clone(b.Passthrough.Paths)
		b.Passthrough.Query = slices.Clone(b.Passthrough.Query)
		b.Passthrough.Headers = slices.Clone(b.Passthrough.Headers)
	}
	b.Budget = copyPointer(b.Budget)
	if b.Budget != nil {
		b.Budget.DailyCostLimit = copyPointer(b.Budget.DailyCostLimit)
		b.Budget.MonthlyCostLimit = copyPointer(b.Budget.MonthlyCostLimit)
	}
	return b
}
func clonePredicate(p Predicate) Predicate {
	p.Operations = slices.Clone(p.Operations)
	p.MinInputTokens = copyPointer(p.MinInputTokens)
	p.MaxInputTokens = copyPointer(p.MaxInputTokens)
	p.MinOutputTokens = copyPointer(p.MinOutputTokens)
	p.MaxOutputTokens = copyPointer(p.MaxOutputTokens)
	p.Streaming = copyPointer(p.Streaming)
	p.Tools = copyPointer(p.Tools)
	p.Modalities = slices.Clone(p.Modalities)
	p.StructuredOutput = copyPointer(p.StructuredOutput)
	p.ReasoningEffort = slices.Clone(p.ReasoningEffort)
	p.Plugin = copyPointer(p.Plugin)
	p.Classifier = copyPointer(p.Classifier)
	if p.Classifier != nil {
		p.Classifier.Labels = slices.Clone(p.Classifier.Labels)
		p.Classifier.MinScore = copyPointer(p.Classifier.MinScore)
	}
	return p
}
func cloneCodeRoute(route codemode.Route) codemode.Route {
	return codemode.Route{
		ID: route.ID, ProjectID: route.ProjectID, Slug: route.Slug, PoolID: route.PoolID,
		ModelPools: maps.Collect(func(yield func(string, []string) bool) {
			for public, targets := range route.ModelPools {
				if !yield(public, slices.Clone(targets)) {
					return
				}
			}
		}),
		Models: slices.Clone(route.Models), Aliases: maps.Clone(route.Aliases), ModelRewriting: route.ModelRewriting,
		MessagesToCodex: route.MessagesToCodex, InitialRecoveryAttempts: route.InitialRecoveryAttempts,
		Tools: func() *codemode.ResearchTools {
			if route.Tools == nil {
				return nil
			}
			return &codemode.ResearchTools{AccountID: route.Tools.AccountID, Services: slices.Clone(route.Tools.Services)}
		}(),
		MaxBodyBytes: copyPointer(route.MaxBodyBytes), Enabled: route.Enabled, ETag: route.ETag,
		RevisionID: route.RevisionID, Revision: route.Revision, PublishedAt: copyPointer(route.PublishedAt),
	}
}
func cloneConfiguration(c Configuration) Configuration {
	cfg := connectors.Config{Network: c.Options.Network, BatchStorage: c.BatchStorage, TuningDatasetURIPrefixes: c.Options.TuningDatasetURIPrefixes, DocumentURIPrefixes: c.Options.DocumentURIPrefixes, SemanticHeaders: c.Options.SemanticHeaders, QuerySettings: c.Options.QuerySettings, OperationDefaults: c.Options.OperationDefaults, Bindings: c.Options.Bindings, Models: c.Options.Models, CredentialHeaders: c.Options.CredentialHeaders, PluginOptions: c.Options.PluginOptions}.Clone()
	c.BatchStorage = cfg.BatchStorage
	c.Options.TuningDatasetURIPrefixes = cfg.TuningDatasetURIPrefixes
	c.Options.DocumentURIPrefixes = cfg.DocumentURIPrefixes
	c.Options.Network = cfg.Network
	c.Options.SemanticHeaders = cfg.SemanticHeaders
	c.Options.QuerySettings = cfg.QuerySettings
	c.Options.OperationDefaults = cfg.OperationDefaults
	c.Options.Bindings = cfg.Bindings
	c.Options.Models = cfg.Models
	c.Options.CredentialHeaders = cfg.CredentialHeaders
	c.Options.PluginOptions = cfg.PluginOptions
	c.Options.ParameterDefaults = cloneValues(c.Options.ParameterDefaults)
	c.Options.Limits = cloneLimits(c.Options.Limits)
	c.Options.Commitment = copyPointer(c.Options.Commitment)
	c.Options.HealthProbe = copyPointer(c.Options.HealthProbe)
	return c
}
