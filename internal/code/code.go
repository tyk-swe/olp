package code

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const BasePath = "/code/"

type Adapter string

const (
	AdapterClaude        Adapter = "claude"
	AdapterAntigravity   Adapter = "antigravity"
	AdapterCodex         Adapter = "codex"
	AdapterOpenCodeGo    Adapter = "opencode_go"
	AdapterZAICoding     Adapter = "zai_coding"
	AdapterMiniMaxPlan   Adapter = "minimax_plan"
	AdapterKimiCode      Adapter = "kimi_code"
	AdapterQwenTokenPlan Adapter = "qwen_token_plan"
)

type Protocol string

const (
	ProtocolResponses Protocol = "responses"
	ProtocolMessages  Protocol = "messages"
	ProtocolChat      Protocol = "chat"
)

type Transport uint8

const (
	HTTP Transport = 1 << iota
	WebSocket
)

type Health string

const (
	HealthUnknown      Health = "unknown"
	HealthHealthy      Health = "healthy"
	HealthUnavailable  Health = "unavailable"
	HealthQuotaLimited Health = "quota_limited"
)

type GrantState string

const (
	GrantCurrent GrantState = "current"
	GrantExpired GrantState = "expired"
	GrantLapsed  GrantState = "lapsed"
	GrantRevoked GrantState = "revoked"
)

type AttemptState string

const (
	StatePrepared       AttemptState = "prepared"
	StateUncertain      AttemptState = "uncertain"
	StateSettled        AttemptState = "settled"
	StateBoundViolation AttemptState = "bound_violation"
	StateAborted        AttemptState = "aborted"
)

type PoolKind string

const (
	PoolPersonal PoolKind = "personal"
	PoolShared   PoolKind = "shared"
)

type Client = string

const (
	ClientCodex      Client = "codex"
	ClientClaudeCode Client = "claude-code"
	ClientOpenCode   Client = "opencode"
)

type Platform = string

const (
	PlatformLinux   Platform = "linux"
	PlatformMacOS   Platform = "macos"
	PlatformWindows Platform = "windows"
)

type Dispatch struct {
	ClientPath string
	Adapter    Adapter
	Protocol   Protocol
}

type Authorization struct {
	Headers         http.Header
	Principal       string
	CredentialID    string
	GrantGeneration int64
	ProjectID       string
}

type Refusal struct {
	Status  int
	Code    string
	RetryAt *time.Time
}

func (r *Refusal) Error() string           { return r.Code }
func Refuse(status int, code string) error { return &Refusal{Status: status, Code: code} }

func (r *Refusal) RetryAfter(now time.Time) time.Duration {
	if r == nil || r.RetryAt == nil || !r.RetryAt.After(now) {
		return 0
	}
	return time.Duration(math.Ceil(r.RetryAt.Sub(now).Seconds())) * time.Second
}

type Account struct {
	ID                       string     `json:"id"`
	ProjectID                string     `json:"project_id"`
	ProviderID               string     `json:"provider_id"`
	CredentialID             string     `json:"credential_id"`
	Principal                string     `json:"principal"`
	Name                     string     `json:"name"`
	Enabled                  bool       `json:"enabled"`
	Models                   []string   `json:"models"`
	ExtraUsageEnabled        *bool      `json:"extra_usage_enabled"`
	ETag                     string     `json:"etag"`
	Health                   Health     `json:"health"`
	HealthObservedAt         *time.Time `json:"health_observed_at,omitempty"`
	UnavailableUntil         *time.Time `json:"unavailable_until,omitempty"`
	Eligible                 bool       `json:"eligible"`
	GrantState               GrantState `json:"grant_state"`
	Allowance                *Allowance `json:"allowance"`
	Cooldowns                []Cooldown `json:"cooldowns,omitempty"`
	CycleAnchor              *string    `json:"cycle_anchor,omitempty"`
	NextMonthlyReset         *string    `json:"next_monthly_reset,omitempty"`
	AllowanceReadRequestedAt *time.Time `json:"allowance_read_requested_at,omitempty"`
	AllowanceReadAt          *time.Time `json:"allowance_read_at,omitempty"`
}

type ClientConfiguration struct {
	PaidModels        []string            `json:"paid_models,omitempty"`
	Models            []Model             `json:"models"`
	AdditionalFiles   []ConfigurationFile `json:"additional_files,omitempty"`
	RouteSlug         string              `json:"route_slug"`
	BaseURL           string              `json:"base_url"`
	NativeModels      []string            `json:"native_models"`
	Adapters          []Adapter           `json:"adapters"`
	Client            string              `json:"client"`
	Platform          string              `json:"platform"`
	Instructions      string              `json:"instructions"`
	SupportedClients  []string            `json:"supported_clients"`
	ClientVersion     string              `json:"client_version"`
	Model             string              `json:"model"`
	PlanModel         *string             `json:"plan_model"`
	SmallModel        *string             `json:"small_model"`
	Format            string              `json:"format"`
	File              *string             `json:"file"`
	Configuration     string              `json:"configuration"`
	QualificationGaps []string            `json:"qualification_gaps"`
}

type Model struct {
	ModelPool    []string          `json:"model_pool,omitempty"`
	Paid         bool              `json:"paid"`
	ID           string            `json:"id"`
	Object       string            `json:"object"`
	Capabilities []ModelCapability `json:"capabilities"`
	Clients      []string          `json:"clients"`
}

type ModelCapability struct {
	Protocol   Protocol `json:"protocol"`
	Path       string   `json:"path"`
	Transports []string `json:"transports"`
}

type CandidateMatch struct {
	ID             string
	ProjectID      string
	ScopeMatches   bool
	NaturalMatches bool
}

type PoolOwnership struct {
	kind  PoolKind
	owner *string
}

func SharedPool() PoolOwnership { return PoolOwnership{kind: PoolShared} }

func PersonalPool(ownerUserID string) (PoolOwnership, error) {
	if ownerUserID == "" {
		return PoolOwnership{}, fmt.Errorf("personal pool requires an owner")
	}
	return PoolOwnership{kind: PoolPersonal, owner: &ownerUserID}, nil
}

func ParsePoolOwnership(kind PoolKind, owner *string) (PoolOwnership, error) {
	switch kind {
	case PoolShared:
		if owner != nil {
			return PoolOwnership{}, fmt.Errorf("shared pools have no owner")
		}
		return SharedPool(), nil
	case PoolPersonal:
		if owner == nil || *owner == "" {
			return PoolOwnership{}, fmt.Errorf("personal pools require an owner")
		}
		return PersonalPool(*owner)
	default:
		return PoolOwnership{}, fmt.Errorf("unknown pool kind")
	}
}

func (o PoolOwnership) Kind() PoolKind { return o.kind }
func (o PoolOwnership) Owner() *string { return o.owner }

type Pool struct {
	ID             string           `json:"id"`
	ProjectID      string           `json:"project_id"`
	Name           string           `json:"name"`
	Kind           PoolKind         `json:"kind"`
	OwnerUserID    *string          `json:"owner_user_id"`
	AccountIDs     []string         `json:"account_ids"`
	AccountWeights map[string]int64 `json:"account_weights"`
	APIKeyIDs      []string         `json:"api_key_ids"`
	ETag           string           `json:"etag"`
}

type Identity struct {
	Conversation string
	Parent       string
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

func (i Identity) Validate() error {
	if !identifier.MatchString(i.Conversation) || i.Parent != "" && (!identifier.MatchString(i.Parent) || i.Parent == i.Conversation) {
		return Refuse(400, "code_identity_invalid")
	}
	return nil
}

func (i Identity) Generic() bool {
	return strings.HasPrefix(i.Conversation, "olp:request/") || strings.HasPrefix(i.Conversation, "olp:affinity/")
}

type Pin struct {
	TargetModel string    `json:"target_model"`
	Model       string    `json:"model"`
	AccountID   string    `json:"account_id"`
	Principal   string    `json:"principal"`
	CreatedAt   time.Time `json:"created_at"`
}

type Binding struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	RouteID      string     `json:"route_id"`
	APIKeyID     string     `json:"api_key_id"`
	Conversation string     `json:"conversation"`
	ParentID     *string    `json:"parent_id"`
	RootID       string     `json:"root_id"`
	AccountID    string     `json:"account_id"`
	Principal    string     `json:"principal"`
	CreatedAt    time.Time  `json:"created_at"`
	RetiredAt    *time.Time `json:"retired_at"`
}

type Operation struct {
	RequestedModel string
	Name           string
	Model          string
	Identity       Identity
}

func (o Operation) PublicModel() string {
	if o.RequestedModel != "" {
		return o.RequestedModel
	}
	return o.Model
}

type Request struct {
	Recoverable      bool
	Operation        Operation
	PreviousResponse string
	Estimate         int64
}

type TokenBound struct {
	Tokens   int64
	Evidence string
}

func (b *TokenBound) Validate() error {
	if b == nil || b.Tokens < 1 || b.Tokens > 1<<53-1 || b.Evidence == "" || len(b.Evidence) > 256 {
		return Refuse(422, "code_token_bound_unavailable")
	}
	return nil
}

type CredentialRefreshTarget struct {
	AccountID       string
	CredentialID    string
	GrantGeneration int64
	RejectedUntil   time.Time
}

type Recovery struct {
	From       string
	Envelope   string
	Number     int
	credential *CredentialRefreshTarget
}

func NewRebindRecovery(from, envelope string, number int) Recovery {
	return Recovery{From: from, Envelope: envelope, Number: number}
}

func NewCredentialRecovery(from, envelope string, number int, target CredentialRefreshTarget) Recovery {
	return Recovery{From: from, Envelope: envelope, Number: number, credential: &target}
}

func (r Recovery) CredentialRefresh() (CredentialRefreshTarget, bool) {
	if r.credential == nil {
		return CredentialRefreshTarget{}, false
	}
	return *r.credential, true
}

type Attempt struct {
	InitialRecovery   bool              `json:"initial_recovery"`
	Nonexecution      bool              `json:"nonexecution"`
	RecoveryFrom      *string           `json:"recovery_from"`
	RecoveryReason    *string           `json:"recovery_reason"`
	RequestedModel    *string           `json:"requested_model"`
	Attribution       map[string]string `json:"attribution,omitempty"`
	EndUserDigest     string            `json:"end_user_digest"`
	UpstreamStatus    *int              `json:"upstream_status"`
	OutcomeOrigin     *OutcomeOrigin    `json:"outcome_origin"`
	Outcome           *OutcomeKind      `json:"outcome"`
	OutcomeObservedAt *time.Time        `json:"outcome_observed_at"`
	ID                string            `json:"id"`
	ProjectID         string            `json:"project_id"`
	RouteID           string            `json:"route_id"`
	RouteRevisionID   string            `json:"route_revision_id"`
	APIKeyID          string            `json:"api_key_id"`
	BindingID         string            `json:"binding_id"`
	AccountID         string            `json:"account_id"`
	Operation         string            `json:"operation"`
	Model             string            `json:"model"`
	State             AttemptState      `json:"state"`
	ReservedTokens    int64             `json:"reserved_tokens"`
	ReportedTokens    *int64            `json:"reported_tokens"`
	InputTokens       *int64            `json:"input_tokens"`
	OutputTokens      *int64            `json:"output_tokens"`
	CachedTokens      *int64            `json:"cached_tokens"`
	ReasoningTokens   *int64            `json:"reasoning_tokens"`
	BoundEvidence     *string           `json:"bound_evidence"`
	Refusal           *string           `json:"refusal"`
	AllowanceCharge   json.RawMessage   `json:"allowance_charge"`
	CreatedAt         time.Time         `json:"created_at"`
	FinishedAt        *time.Time        `json:"finished_at"`
}

// Total includes Cached input and Reasoning output as subsets, never additional charges.
type Usage struct {
	Total     *int64
	Input     *int64
	Output    *int64
	Cached    *int64
	Reasoning *int64
}

func (u Usage) Validate() error {
	for _, n := range []*int64{u.Total, u.Input, u.Output, u.Cached, u.Reasoning} {
		if n != nil && (*n < 0 || *n > 1<<53-1) {
			return fmt.Errorf("invalid token usage")
		}
	}
	if u.Total != nil && u.Input != nil && u.Output != nil && *u.Input+*u.Output != *u.Total {
		return fmt.Errorf("inconsistent token usage")
	}
	if u.Cached != nil && u.Input != nil && *u.Cached > *u.Input {
		return fmt.Errorf("inconsistent cached usage")
	}
	if u.Reasoning != nil && u.Output != nil && *u.Reasoning > *u.Output {
		return fmt.Errorf("inconsistent reasoning usage")
	}
	return nil
}
