package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type slotRow struct {
	ID                string
	ETag              string
	Default           bool
	Position          int
	Name              string
	Enabled           bool
	Priority          int
	Weight            int64
	CredentialID      *string
	CredentialVersion *int
	CredentialRevoked bool
	// CredentialPlugin is the digest of the plugin build whose grant
	// enrollment created the credential version, or "" for a pasted one.
	CredentialPlugin    string
	CredentialProfile   string
	CredentialLapsed    bool
	GrantGeneration     int64
	CredentialPrincipal string
	// CredentialFacts are what the credential version reported serving as at
	// enrollment, beyond its principal.
	CredentialFacts      map[string]string
	Restrictions         SlotRestrictions
	Limits               connection.Limits
	ValidatedAt          *time.Time
	ValidatedFingerprint *string
}

type SlotRestrictions struct {
	AllowedAPIKeys []string `json:"allowed_api_keys"`
	AllowedModels  []string `json:"allowed_models"`
	AllowedRoutes  []string `json:"allowed_routes"`
}

func findSlot(slots []slotRow, id string) *slotRow {
	index := slices.IndexFunc(slots, func(row slotRow) bool { return row.ID == id })
	if index < 0 {
		return nil
	}
	return &slots[index]
}

func loadSlots(ctx context.Context, q database.Queryer, providerID string) ([]slotRow, error) {
	rows, err := q.Query(ctx, "SELECT s.id::text,s.is_default,s.position,s.name,s.enabled,s.priority,s.weight,s.credential_id::text,c.version,c.revoked_at IS NOT NULL,coalesce(c.plugin_digest,''),coalesce(c.profile_id,''),g.lapsed_at IS NOT NULL,coalesce(g.generation,0),coalesce(c.principal,''),coalesce(c.grant_facts,'{}'),s.restrictions,s.limits,s.validated_at,s.validated_fingerprint,s.etag::text FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id LEFT JOIN olp.provider_grants g ON g.credential_id=s.credential_id WHERE s.provider_id=$1 ORDER BY s.position", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []slotRow
	for rows.Next() {
		var row slotRow
		var facts, restrictions, limits []byte
		var revoked *bool
		if err = rows.Scan(&row.ID, &row.Default, &row.Position, &row.Name, &row.Enabled, &row.Priority, &row.Weight, &row.CredentialID, &row.CredentialVersion, &revoked, &row.CredentialPlugin, &row.CredentialProfile, &row.CredentialLapsed, &row.GrantGeneration, &row.CredentialPrincipal, &facts, &restrictions, &limits, &row.ValidatedAt, &row.ValidatedFingerprint, &row.ETag); err != nil {
			return nil, err
		}
		row.CredentialRevoked = revoked != nil && *revoked
		if err = json.Unmarshal(facts, &row.CredentialFacts); err == nil {
			err = json.Unmarshal(restrictions, &row.Restrictions)
		}
		if err == nil {
			err = json.Unmarshal(limits, &row.Limits)
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, row)
	}
	return slots, rows.Err()
}

const maxSlots = 64

type slotInput struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Enabled               *bool    `json:"enabled"`
	Priority              *int     `json:"priority"`
	Weight                *int64   `json:"weight"`
	CredentialVersionID   *string  `json:"credential_version_id"`
	AllowedAPIKeys        []string `json:"allowed_api_keys"`
	AllowedModels         []string `json:"allowed_models"`
	AllowedRoutes         []string `json:"allowed_routes"`
	MaxConcurrency        *int64   `json:"max_concurrency"`
	RequestsPerMinute     *int64   `json:"requests_per_minute"`
	TokensPerMinute       *int64   `json:"tokens_per_minute"`
	InputTokensPerMinute  *int64   `json:"input_tokens_per_minute"`
	OutputTokensPerMinute *int64   `json:"output_tokens_per_minute"`
	routing.Supply
}

func (in slotInput) limits() connection.Limits {
	return connection.Limits{MaxConcurrency: in.MaxConcurrency, RequestsPerMinute: in.RequestsPerMinute, TokensPerMinute: in.TokensPerMinute, InputTokensPerMinute: in.InputTokensPerMinute, OutputTokensPerMinute: in.OutputTokensPerMinute, Supply: in.Supply}
}

type slotWrite struct {
	Slot                slotInput              `json:"slot"`
	Credential          *string                `json:"credential"`
	CredentialReference *secretstore.Reference `json:"credential_reference,omitempty"`
}

type slotReply struct {
	ID                    string          `json:"id"`
	Name                  string          `json:"name"`
	Enabled               bool            `json:"enabled"`
	Priority              int             `json:"priority"`
	Weight                int64           `json:"weight"`
	CredentialVersionID   *string         `json:"credential_version_id"`
	AllowedAPIKeys        []string        `json:"allowed_api_keys"`
	AllowedModels         []string        `json:"allowed_models"`
	AllowedRoutes         []string        `json:"allowed_routes"`
	MaxConcurrency        *int64          `json:"max_concurrency"`
	RequestsPerMinute     *int64          `json:"requests_per_minute"`
	TokensPerMinute       *int64          `json:"tokens_per_minute"`
	InputTokensPerMinute  *int64          `json:"input_tokens_per_minute"`
	OutputTokensPerMinute *int64          `json:"output_tokens_per_minute"`
	DailyCostLimit        *string         `json:"daily_cost_limit"`
	MonthlyCostLimit      *string         `json:"monthly_cost_limit"`
	PriorityShares        *routing.Shares `json:"priority_shares"`
	SaturationPercent     *int64          `json:"saturation_percent"`
}

func (row slotRow) reply() slotReply {
	return slotReply{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority, Weight: row.Weight,
		CredentialVersionID: row.CredentialID,
		AllowedAPIKeys:      orEmpty(row.Restrictions.AllowedAPIKeys), AllowedModels: orEmpty(row.Restrictions.AllowedModels), AllowedRoutes: orEmpty(row.Restrictions.AllowedRoutes),
		MaxConcurrency: row.Limits.MaxConcurrency, RequestsPerMinute: row.Limits.RequestsPerMinute, TokensPerMinute: row.Limits.TokensPerMinute,
		InputTokensPerMinute: row.Limits.InputTokensPerMinute, OutputTokensPerMinute: row.Limits.OutputTokensPerMinute,
		DailyCostLimit: row.Limits.DailyCostLimit, MonthlyCostLimit: row.Limits.MonthlyCostLimit, PriorityShares: row.Limits.PriorityShares, SaturationPercent: row.Limits.SaturationPercent,
	}
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func (s *Server) slotList(ctx context.Context, q database.Queryer, current *record) (httpkit.Reply, error) {
	slots, err := loadSlots(ctx, q, current.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	activeCredentials := map[string]*string{}
	var activeQuotas []routing.SharedTokenQuota
	if current.ActiveRevisionID != nil {
		var encoded, configuration []byte
		if err := q.QueryRow(ctx, "SELECT slots,configuration FROM olp.provider_revisions WHERE id=$1 AND provider_id=$2", *current.ActiveRevisionID, current.ID).Scan(&encoded, &configuration); err != nil {
			return httpkit.Reply{}, err
		}
		var published []routing.RevisionSlot
		if err := json.Unmarshal(encoded, &published); err != nil {
			return httpkit.Reply{}, err
		}
		active, err := connection.Decode(configuration)
		if err != nil {
			return httpkit.Reply{}, err
		}
		activeQuotas = active.Options.SharedTokenQuotas
		for _, slot := range published {
			activeCredentials[slot.ID] = slot.CredentialID
		}
	}
	models, err := enabledModels(ctx, q, current.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var credentialIDs []string
	for _, id := range activeCredentials {
		if id != nil {
			credentialIDs = append(credentialIDs, *id)
		}
	}
	generations, err := release.ReadGrantGenerations(ctx, q, credentialIDs)
	if err != nil {
		return httpkit.Reply{}, err
	}
	live, connection := s.quotas(ctx, current.ID, slots, activeCredentials, generations)
	groups := s.sharedGroupUsage(ctx, current.ProjectID, activeQuotas)
	items := make([]slotReply, len(slots))
	health := make(map[string]slotHealth, len(slots))
	for i, row := range slots {
		items[i] = row.reply()
		quota := live[row.ID]
		health[row.ID] = slotHealth{
			Revoked: row.CredentialRevoked, Lapsed: row.CredentialLapsed, ActiveCredentialVersionID: activeCredentials[row.ID],
			CoolingDown: quota.coolingDown, ValidatedAt: row.validationTime(current.Connection.Document(), models), Usage: quota.usage,
		}
	}
	return httpkit.Detail(slotList{Items: items, Health: health, ETag: current.SlotsETag, ConnectionUsage: connection, SharedGroupUsage: groups}, current.SlotsETag), nil
}

type slotList struct {
	Items            []slotReply                  `json:"items"`
	Health           map[string]slotHealth        `json:"health"`
	ETag             string                       `json:"etag"`
	ConnectionUsage  *quotaUsage                  `json:"connection_usage"`
	SharedGroupUsage map[string]*sharedGroupUsage `json:"shared_group_usage"`
}

type sharedGroupUsage struct {
	TokensThisMinute int64 `json:"tokens_this_minute"`
	TokensPerMinute  int64 `json:"tokens_per_minute"`
}

func (s *Server) sharedGroupUsage(ctx context.Context, projectID *string, quotas []routing.SharedTokenQuota) map[string]*sharedGroupUsage {
	groups := map[string]*sharedGroupUsage{}
	if s.quota == nil || projectID == nil || len(quotas) == 0 {
		return groups
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	for _, quota := range quotas {
		if _, seen := groups[quota.Group]; seen {
			continue
		}
		lookup := limits.SharedQuotaLookup(routing.SharedTokenQuotaGroupID(*projectID, quota.Group))
		usage, err := s.quota.Usage(ctx, lookup)
		if err != nil {
			s.quotaUnavailable(err)
			return groups
		}
		groups[quota.Group] = &sharedGroupUsage{TokensThisMinute: usage.TokensThisMinute, TokensPerMinute: quota.TokensPerMinute}
	}
	return groups
}

type slotHealth struct {
	Revoked                   bool        `json:"revoked"`
	Lapsed                    bool        `json:"lapsed"`
	ActiveCredentialVersionID *string     `json:"active_credential_version_id"`
	CoolingDown               *bool       `json:"cooling_down"`
	ValidatedAt               *time.Time  `json:"validated_at"`
	Usage                     *quotaUsage `json:"usage"`
}

// A slow quota store costs the slot list its live counters, never the list.
const quotaTimeout = 2 * time.Second

type quotaUsage struct {
	RequestsThisMinute     int64 `json:"requests_this_minute"`
	TokensThisMinute       int64 `json:"tokens_this_minute"`
	InputTokensThisMinute  int64 `json:"input_tokens_this_minute"`
	OutputTokensThisMinute int64 `json:"output_tokens_this_minute"`
	ConcurrentRequests     int64 `json:"concurrent_requests"`
}

func usageOf(u fleet.Usage) *quotaUsage {
	return &quotaUsage{
		RequestsThisMinute: u.RequestsThisMinute, TokensThisMinute: u.TokensThisMinute,
		InputTokensThisMinute: u.InputTokensThisMinute, OutputTokensThisMinute: u.OutputTokensThisMinute,
		ConcurrentRequests: u.ConcurrentRequests,
	}
}

type slotQuota struct {
	usage       *quotaUsage
	coolingDown *bool
}

// A counter that cannot be read stays nil, so the console shows it as unknown
// instead of an idle zero or a failed request.
func (s *Server) quotas(ctx context.Context, providerID string, slots []slotRow, published map[string]*string, generations map[string]int64) (map[string]slotQuota, *quotaUsage) {
	if s.quota == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	connection, err := s.quota.Usage(ctx, limits.ConnectionLookup(providerID))
	if err != nil {
		s.quotaUnavailable(err)
		return nil, nil
	}
	live := make(map[string]slotQuota, len(slots))
	for _, row := range slots {
		usage, err := s.quota.Usage(ctx, limits.SlotLookup(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		// The cooldown is asked of the credential version the published
		// revision dispatches with, because that is the scope the gateway
		// penalises, and of the slot itself.
		var generation int64
		if id := published[row.ID]; id != nil {
			generation = generations[*id]
		}
		cooling, err := s.quota.Cooling(ctx, fleet.CredentialScope(providerID, published[row.ID], generation), fleet.SlotScope(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		live[row.ID] = slotQuota{usage: usageOf(usage), coolingDown: &cooling}
	}
	return live, usageOf(connection)
}

func (s *Server) quotaUnavailable(err error) {
	s.log.Warn("shared provider quota state is unavailable", "error", err)
}

func (s *Server) slots(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	current, err := visibleProvider(r.Context(), s.access.Pool, p, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.slotList(r.Context(), s.access.Pool, current)
}
