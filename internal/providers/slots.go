package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

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
		AllowedAPIKeys:      ptr.NonNil(row.Restrictions.AllowedAPIKeys), AllowedModels: ptr.NonNil(row.Restrictions.AllowedModels), AllowedRoutes: ptr.NonNil(row.Restrictions.AllowedRoutes),
		MaxConcurrency: row.Limits.MaxConcurrency, RequestsPerMinute: row.Limits.RequestsPerMinute, TokensPerMinute: row.Limits.TokensPerMinute,
		InputTokensPerMinute: row.Limits.InputTokensPerMinute, OutputTokensPerMinute: row.Limits.OutputTokensPerMinute,
		DailyCostLimit: row.Limits.DailyCostLimit, MonthlyCostLimit: row.Limits.MonthlyCostLimit, PriorityShares: row.Limits.PriorityShares, SaturationPercent: row.Limits.SaturationPercent,
	}
}

func (s *slotService) slotList(ctx context.Context, q database.Queryer, current *record) (httpkit.Reply, error) {
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
	live, connectionUsage := s.quotas(ctx, current.ID, slots, activeCredentials, generations)
	groups := s.sharedGroupUsage(ctx, current.ProjectID, activeQuotas)
	items := make([]slotReply, len(slots))
	health := make(map[string]slotHealth, len(slots))
	for i, row := range slots {
		items[i] = row.reply()
		quota := live[row.ID]
		validatedAt, err := row.validationTime(current.Connection.Document(), models)
		if err != nil {
			return httpkit.Reply{}, err
		}
		health[row.ID] = slotHealth{
			Revoked: row.CredentialRevoked, Lapsed: row.CredentialLapsed, ActiveCredentialVersionID: activeCredentials[row.ID],
			CoolingDown: quota.coolingDown, ValidatedAt: validatedAt, Usage: quota.usage,
		}
	}
	return httpkit.Detail(slotList{Items: items, Health: health, ETag: current.SlotsETag, ConnectionUsage: connectionUsage, SharedGroupUsage: groups}, current.SlotsETag), nil
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

func (s *slotService) sharedGroupUsage(ctx context.Context, projectID *string, quotas []routing.SharedTokenQuota) map[string]*sharedGroupUsage {
	groups := map[string]*sharedGroupUsage{}
	if s.quota == nil || projectID == nil || len(quotas) == 0 {
		return groups
	}
	quotaCtx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	for _, quota := range quotas {
		if _, seen := groups[quota.Group]; seen {
			continue
		}
		lookup := limits.SharedQuotaLookup(routing.SharedTokenQuotaGroupID(*projectID, quota.Group))
		usage, err := s.quota.Usage(quotaCtx, lookup)
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
func (s *slotService) quotas(ctx context.Context, providerID string, slots []slotRow, published map[string]*string, generations map[string]int64) (map[string]slotQuota, *quotaUsage) {
	if s.quota == nil {
		return nil, nil
	}
	quotaCtx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	connectionUsage, err := s.quota.Usage(quotaCtx, limits.ConnectionLookup(providerID))
	if err != nil {
		s.quotaUnavailable(err)
		return nil, nil
	}
	live := make(map[string]slotQuota, len(slots))
	for _, row := range slots {
		usage, err := s.quota.Usage(quotaCtx, limits.SlotLookup(row.ID))
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
		cooling, err := s.quota.Cooling(quotaCtx, fleet.CredentialScope(providerID, published[row.ID], generation), fleet.SlotScope(row.ID))
		if err != nil {
			s.quotaUnavailable(err)
			break
		}
		live[row.ID] = slotQuota{usage: usageOf(usage), coolingDown: &cooling}
	}
	return live, usageOf(connectionUsage)
}

func (s *slotService) quotaUnavailable(err error) {
	s.log.Warn("shared provider quota state is unavailable", "error", err)
}

func (s *slotService) slots(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
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

func (s *slotService) validateSlot(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	slotID, err := httpkit.IDParam(r, "slot_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	input, credential, err := s.prepareSlotValidation(r.Context(), p, id, slotID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer clear(credential)
	current, slot, models := input.provider, input.slot, input.models
	probeErr := validateModelAccess(r.Context(), s.prober, current.target(credential), *slot, models)
	// Probes run without mutation locks. Only save evidence if its complete
	// input (credential, transport, restrictions, and models) is still current.
	result, err := access.Transact(s.access, r, unchangedSince(r.Context(), current, "validation"), func(tx pgx.Tx, p authz.Principal, locked *record) (httpkit.Reply, access.Event, error) {
		if err := s.storeSlotValidation(r.Context(), tx, current, slot, models, probeErr == nil); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := s.slotList(r.Context(), tx, locked)
		return reply, access.Event{Action: "provider.slot.validate", Resource: "provider_slot", ID: slotID, Failed: probeErr != nil}, err
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if probeErr != nil {
		return result, problem.Fail(422, "slot_validation_failed", probe.Classify(probeErr).Detail)
	}
	clearValidatedCooldowns(r.Context(), s.quota, s.log, id, *slot)
	return result, nil
}
