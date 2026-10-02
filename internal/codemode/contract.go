// Package codemode defines the observation and management contracts of coding
// traffic. Observations never replace the bytes a transport forwards.
package codemode

import (
	"fmt"
	"math"
	"regexp"
	"time"
)

const BasePath = "/code/"

type Refusal struct {
	Status int
	Code   string
}

func (r *Refusal) Error() string           { return r.Code }
func Refuse(status int, code string) error { return &Refusal{Status: status, Code: code} }

type Account struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	ProviderID   string     `json:"provider_id"`
	CredentialID string     `json:"credential_id"`
	Principal    string     `json:"principal"`
	Name         string     `json:"name"`
	Enabled      bool       `json:"enabled"`
	Models       []string   `json:"models"`
	ETag         string     `json:"etag"`
	Health       string     `json:"health"`
	Eligible     bool       `json:"eligible"`
	GrantState   string     `json:"grant_state"`
	Allowance    *Allowance `json:"allowance"`
}

// Allowance is provider-reported subscription metadata, never a local budget.
type Allowance struct {
	RemainingTokens   *int64     `json:"remaining_tokens"`
	RemainingRequests *int64     `json:"remaining_requests"`
	RemainingPercent  *float64   `json:"remaining_percent"`
	ResetsAt          *time.Time `json:"resets_at"`
	ObservedAt        time.Time  `json:"observed_at"`
}

func (a Allowance) Validate() error {
	for _, n := range []*int64{a.RemainingTokens, a.RemainingRequests} {
		if n != nil && (*n < 0 || *n > 1<<53-1) {
			return fmt.Errorf("invalid provider allowance")
		}
	}
	if a.ObservedAt.IsZero() || a.RemainingPercent != nil && (math.IsNaN(*a.RemainingPercent) || math.IsInf(*a.RemainingPercent, 0) || *a.RemainingPercent < 0 || *a.RemainingPercent > 100) {
		return fmt.Errorf("invalid provider allowance")
	}
	return nil
}

type ClientConfiguration struct {
	RouteSlug         string   `json:"route_slug"`
	BaseURL           string   `json:"base_url"`
	NativeModels      []string `json:"native_models"`
	Client            string   `json:"client"`
	ClientVersion     string   `json:"client_version"`
	Configuration     string   `json:"configuration"`
	QualificationGaps []string `json:"qualification_gaps"`
}

type Pool struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	OwnerUserID *string  `json:"owner_user_id"`
	AccountIDs  []string `json:"account_ids"`
	APIKeyIDs   []string `json:"api_key_ids"`
	ETag        string   `json:"etag"`
}

type Route struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	Slug        string     `json:"slug"`
	PoolID      string     `json:"pool_id"`
	Models      []string   `json:"models"`
	Enabled     bool       `json:"enabled"`
	RevisionID  string     `json:"revision_id"`
	Revision    int        `json:"revision"`
	ETag        string     `json:"etag"`
	PublishedAt *time.Time `json:"published_at"`
}

func (r Route) BasePath() string { return BasePath + r.Slug }

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
	Name     string
	Model    string
	Identity Identity
}

func ValidateModels(models []string) error {
	if len(models) < 1 || len(models) > 100 {
		return fmt.Errorf("use between 1 and 100 native models")
	}
	seen := map[string]bool{}
	for _, model := range models {
		if !identifier.MatchString(model) || len(model) > 200 || seen[model] {
			return fmt.Errorf("use unique native model identifiers")
		}
		seen[model] = true
	}
	return nil
}

// TokenBound is supplied only by a qualified operation adapter, never by a
// client or operator. Evidence identifies the audited model/operation policy.
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

type Budget struct {
	ID            string  `json:"id"`
	ProjectID     string  `json:"project_id"`
	RouteID       *string `json:"route_id"`
	APIKeyID      *string `json:"api_key_id"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
	ETag          string  `json:"etag"`
}

type Attempt struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	RouteID         string     `json:"route_id"`
	RouteRevisionID string     `json:"route_revision_id"`
	APIKeyID        string     `json:"api_key_id"`
	BindingID       string     `json:"binding_id"`
	AccountID       string     `json:"account_id"`
	Operation       string     `json:"operation"`
	Model           string     `json:"model"`
	State           string     `json:"state"`
	ReservedTokens  int64      `json:"reserved_tokens"`
	ReportedTokens  *int64     `json:"reported_tokens"`
	InputTokens     *int64     `json:"input_tokens"`
	OutputTokens    *int64     `json:"output_tokens"`
	CachedTokens    *int64     `json:"cached_tokens"`
	ReasoningTokens *int64     `json:"reasoning_tokens"`
	BoundEvidence   *string    `json:"bound_evidence"`
	Refusal         *string    `json:"refusal"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

// Usage's total includes cached input and reasoning output; the breakdowns
// are subsets, not additional charges. Nil total means consumption is unknown.
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
