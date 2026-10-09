package access

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
)

// AdmissionLimits is a complete limit set. Nil dimensions are unlimited.
type AdmissionLimits struct {
	RequestsPerMinute *int64  `json:"requests_per_minute"`
	TokensPerMinute   *int64  `json:"tokens_per_minute"`
	MaxConcurrency    *int64  `json:"max_concurrency"`
	DailyCostLimit    *string `json:"daily_cost_limit"`
	MonthlyCostLimit  *string `json:"monthly_cost_limit"`
	WeeklyCostLimit   *string `json:"weekly_cost_limit"`
}

func (p AdmissionLimits) Limited() bool {
	return p.RequestsPerMinute != nil || p.TokensPerMinute != nil || p.MaxConcurrency != nil || p.CostBudgeted()
}

func (p AdmissionLimits) CostBudgeted() bool {
	return p.DailyCostLimit != nil || p.MonthlyCostLimit != nil || p.WeeklyCostLimit != nil
}

// EndUserPolicy applies independently at each boundary. An override replaces
// the complete default set; it never removes a policy on another boundary.
type EndUserPolicy struct {
	LimitTemplate *string                    `json:"limit_template,omitempty"`
	Defaults      AdmissionLimits            `json:"defaults"`
	Overrides     map[string]AdmissionLimits `json:"overrides,omitempty"`
	Blocked       []string                   `json:"blocked,omitempty"`
}

var endUserDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidEndUserDigest accepts only a derived identity, never a raw identifier.
func ValidEndUserDigest(value string) bool { return endUserDigest.MatchString(value) }

func (p *EndUserPolicy) Limits(digest string) AdmissionLimits {
	if p == nil {
		return AdmissionLimits{}
	}
	if override, ok := p.Overrides[digest]; ok {
		return override
	}
	return p.Defaults
}

func (p *EndUserPolicy) Blocks(digest string) bool {
	return p != nil && slices.Contains(p.Blocked, digest)
}

// AllowsEndUser rechecks an established session against current authority.
// Enabling a policy also requires previously unidentified sessions to reconnect.
func (a Authority) AllowsEndUser(digest string) bool {
	if a.Policy.EndUserPolicy == nil && a.ProjectEndUserPolicy == nil {
		return true
	}
	return digest != "" && !a.Policy.EndUserPolicy.Blocks(digest) && !a.ProjectEndUserPolicy.Blocks(digest)
}

func (policy AdmissionLimits) Validate() error {
	return validateKey(keyInput{Name: "end-user policy", KeyPolicy: KeyPolicy{
		Scopes: []string{"inference"}, AllowedRoutes: []string{},
		RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute,
		MaxConcurrency: policy.MaxConcurrency, DailyCostLimit: policy.DailyCostLimit, MonthlyCostLimit: policy.MonthlyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit,
	}}, false)
}

func (p *EndUserPolicy) validate() error {
	if p == nil {
		return nil
	}
	if err := validTemplateName(p.LimitTemplate); err != nil {
		return err
	}
	if len(p.Overrides) > 256 || len(p.Blocked) > 256 {
		return Invalid("end_user_policy", "At most 256 overrides and 256 blocked digests are supported.")
	}

	if err := p.Defaults.Validate(); err != nil {
		return err
	}
	for digest, policy := range p.Overrides {
		if !endUserDigest.MatchString(digest) {
			return Invalid("end_user_policy.overrides", "Use project-scoped SHA-256 digests as override keys.")
		}
		if err := policy.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(p.Blocked))
	for _, digest := range p.Blocked {
		if !endUserDigest.MatchString(digest) || seen[digest] {
			return Invalid("end_user_policy.blocked", "Use unique project-scoped SHA-256 digests.")
		}
		seen[digest] = true
	}
	return nil
}

func (s *Server) projectEndUserPolicy(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, View); err != nil {
		return Reply{}, err
	}
	var policy json.RawMessage
	var etag string
	if err = s.Pool.QueryRow(r.Context(), "SELECT COALESCE(end_user_policy,'null'::jsonb),etag::text FROM olp.projects WHERE id=$1", id).Scan(&policy, &etag); err != nil {
		return Reply{}, err
	}
	return Detail(map[string]any{"policy": policy, "etag": etag}, etag), nil
}

func (s *Server) putProjectEndUserPolicy(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err = Decode(r, &body); err != nil {
		return Reply{}, err
	}
	if len(body.Policy) == 0 {
		return Reply{}, Invalid("policy", "A policy or explicit null is required.")
	}
	var input struct{ Policy *EndUserPolicy }
	decoder := json.NewDecoder(bytes.NewReader(body.Policy))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input.Policy); err != nil {
		return Reply{}, Invalid("policy", "The end-user policy is invalid.")
	}
	if err = input.Policy.validate(); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, Change); err != nil {
		return Reply{}, err
	}
	etag, err := loadProject(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if err = checkLimitTemplates(r.Context(), tx, &id, endUserTemplate(input.Policy)); err != nil {
		return Reply{}, err
	}
	encoded, err := json.Marshal(input.Policy)
	if err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.projects SET end_user_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now() WHERE id=$1", id, encoded, etag); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.end_user_policy.update", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"policy": input.Policy, "etag": etag}, etag))
}
