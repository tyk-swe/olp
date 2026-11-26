package identity

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
)

const (
	maxFederatedReturnLen = 2048
	expiredCleanupLimit   = 100
)

type roleMapping struct {
	ClaimValue string `json:"claim_value"`
	Role       string `json:"role"`
}

type roleMappingPolicy struct {
	DefaultRole   *string
	EmailMappings []roleMapping
	GroupMappings []roleMapping
}

type authorizationURLResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

type federatedBeginInput struct {
	ReturnTo   string `json:"return_to"`
	Purpose    string `json:"purpose"`
	ResourceID string `json:"resource_id"`
}

func validateRoleMappings(emailMappings, groupMappings []roleMapping, emailField, groupField, invalidDetail string) error {
	type mappingGroup struct {
		field           string
		entries         []roleMapping
		caseInsensitive bool
	}
	groups := []mappingGroup{
		{field: emailField, entries: emailMappings, caseInsensitive: true},
		{field: groupField, entries: groupMappings, caseInsensitive: false},
	}
	for _, g := range groups {
		if len(g.entries) > 500 {
			return httpkit.Invalid(g.field, "Use at most 500 mappings.")
		}
		for i, m := range g.entries {
			duplicate := slices.ContainsFunc(g.entries[:i], func(prev roleMapping) bool {
				return prev.ClaimValue == m.ClaimValue || (g.caseInsensitive && strings.EqualFold(prev.ClaimValue, m.ClaimValue))
			})
			if !authz.ValidRole(m.Role) || httpkit.ValidText(g.field, m.ClaimValue, 254) != nil || duplicate {
				return httpkit.Invalid(g.field, invalidDetail)
			}
		}
	}
	return nil
}

func resolveMappedRole(policy roleMappingPolicy, address string, groups []string) string {
	for _, m := range policy.EmailMappings {
		if strings.EqualFold(m.ClaimValue, address) {
			return m.Role
		}
	}
	selected := ""
	rank := map[string]int{"viewer": 1, "developer": 2, "operator": 3, "owner": 4}
	for _, m := range policy.GroupMappings {
		if slices.Contains(groups, m.ClaimValue) && rank[m.Role] > rank[selected] {
			selected = m.Role
		}
	}
	if selected != "" {
		return selected
	}
	if policy.DefaultRole != nil {
		return *policy.DefaultRole
	}
	return ""
}

func parseFederatedBeginInput(r *http.Request, kind string, allowQuery bool) (federatedBeginInput, error) {
	var input federatedBeginInput
	if allowQuery && r.Method == http.MethodGet {
		input.ReturnTo = r.URL.Query().Get("return_to")
	} else if r.ContentLength != 0 {
		if err := httpkit.Decode(r, &input); err != nil {
			return federatedBeginInput{}, err
		}
	}
	if !safeReturn(input.ReturnTo) || len(input.ReturnTo) > maxFederatedReturnLen {
		return federatedBeginInput{}, httpkit.Invalid("return_to", "Use a local console path.")
	}
	if input.ReturnTo == "" {
		input.ReturnTo = "/"
		if kind == "link" {
			input.ReturnTo = "/settings/profile"
		}
	}
	if kind == "reauthenticate" {
		if err := validatePurpose(input.Purpose, input.ResourceID); err != nil {
			return federatedBeginInput{}, err
		}
	} else if input.Purpose != "" || input.ResourceID != "" {
		return federatedBeginInput{}, httpkit.Invalid("purpose", "This flow has no recent-authentication purpose.")
	}
	return input, nil
}

func reauthenticatedAt(purpose string) string {
	if purpose == "plugin_permit" {
		return "/plugins"
	}
	if purpose == "agent_delegation" {
		return "/agents/grants"
	}
	return "/settings/profile"
}

func reauthenticatedLocation(purpose, resourceID string) string {
	location := reauthenticatedAt(purpose) + "?reauthenticated=" + url.QueryEscape(purpose)
	if resourceID != "" {
		location += "&resource_id=" + url.QueryEscape(resourceID)
	}
	return location
}

func pruneExpiredSecrets(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id IN (SELECT id FROM olp.secrets WHERE expires_at<=now() LIMIT $1)", expiredCleanupLimit)
	return err
}

func authenticateFlowSession(s *access.Server, r *http.Request, tx pgx.Tx, expectedUserID, expectedSessionID, mismatchCode, mismatchMsg string) (authz.Principal, error) {
	p, err := s.Authenticate(r, tx)
	if err != nil {
		return authz.Principal{}, err
	}
	if err = p.Authorize(authz.Self); err != nil {
		return authz.Principal{}, err
	}
	if p.ID != expectedUserID || p.SessionID != expectedSessionID {
		return authz.Principal{}, httpkit.Fail(403, mismatchCode, mismatchMsg)
	}
	return p, nil
}

func syncFederatedAuthority(r *http.Request, tx pgx.Tx, userID, mapped, source string) (changed, allowed bool, err error) {
	var management, role string
	var authorized bool
	err = tx.QueryRow(r.Context(), "SELECT role_management,role,oidc_authorized FROM olp.users WHERE id=$1", userID).Scan(&management, &role, &authorized)
	if err != nil {
		return false, false, err
	}
	if management != source {
		return false, true, nil
	}
	allowed = mapped != ""
	if allowed == authorized && (!allowed || mapped == role) {
		return false, allowed, nil
	}
	effectiveRole := mapped
	if !allowed {
		effectiveRole = role
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET role=$2,oidc_authorized=$3,etag=$4,updated_at=now() WHERE id=$1", userID, effectiveRole, allowed, ids.New()); err != nil {
		return false, false, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return false, false, err
	}
	if !allowed || effectiveRole != "owner" {
		if err = retireIssuedInvitations(r.Context(), tx, r, userID, authz.System, ""); err != nil {
			return false, false, err
		}
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return false, false, err
	}
	err = access.Audit(r.Context(), tx, r, authz.System, "user.role_sync_"+source, "user", userID, "success")
	return true, allowed, err
}
