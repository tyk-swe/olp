package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type oidcIdentityItem struct {
	ID          string     `json:"id"`
	Issuer      string     `json:"issuer"`
	EmailAtLink *string    `json:"email_at_link"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CanUnlink   bool       `json:"can_unlink"`
}

type oidcIdentitiesResponse struct {
	Items                         []oidcIdentityItem `json:"items"`
	LinkingAvailable              bool               `json:"linking_available"`
	HasLocalPassword              bool               `json:"has_local_password"`
	OIDCReauthenticationAvailable bool               `json:"oidc_reauthentication_available"`
}

func mappedRole(c oidcConfiguration, address string, groups []string) string {
	return resolveMappedRole(roleMappingPolicy{DefaultRole: c.DefaultRole, EmailMappings: c.EmailMappings, GroupMappings: c.GroupMappings}, address, groups)
}

// Callers restrict identities to the enabled issuer.
func oidcSignInRole(c oidcConfiguration, locallyManaged bool, role string, data []byte) (string, error) {
	// Both supported token authentication methods require the client secret.
	if !c.HasClientSecret || data == nil {
		return "", nil
	}
	var claims oidcRoleClaims
	if err := json.Unmarshal(data, &claims); err != nil {
		return "", err
	}
	// Pairwise subjects can change between clients. Only a verified identity
	// for this client and these claim names proves a usable sign-in path.
	if claims.ClientID != c.ClientID || claims.EmailClaim != c.EmailClaim || claims.GroupsClaim != c.GroupsClaim {
		return "", nil
	}
	// Reducing requested scopes can remove verified email or role claims.
	// Evidence without scopes must be refreshed by a successful sign-in.
	if len(claims.Scopes) == 0 {
		return "", nil
	}
	for _, scope := range claims.Scopes {
		if !slices.Contains(c.Scopes, scope) {
			return "", nil
		}
	}
	if locallyManaged {
		return role, nil
	}
	return mappedRole(c, claims.Email, claims.Groups), nil
}

func usableOIDCIdentities(r *http.Request, q database.Queryer, p authz.Principal, local bool) (map[string]bool, error) {
	c, err := loadOIDC(r.Context(), q)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !c.Enabled {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(r.Context(), "SELECT id::text,role_claims FROM olp.oidc_identities WHERE user_id=$1 AND issuer=$2", p.ID, c.Issuer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usable := map[string]bool{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		role, err := oidcSignInRole(c, local, p.Role, data)
		if err != nil {
			return nil, err
		}
		if role != "" {
			usable[id] = true
		}
	}
	return usable, rows.Err()
}

func (s *oidcService) oidcIdentities(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var local, localEnabled, enabled, locallyManaged bool
	if err := s.access.Pool.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL,COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),COALESCE((SELECT (document->>'enabled')::boolean FROM olp.oidc_configuration WHERE singleton),false),role_management<>'oidc' FROM olp.users WHERE id=$1", p.ID).Scan(&local, &localEnabled, &enabled, &locallyManaged); err != nil {
		return httpkit.Reply{}, err
	}
	usable, err := usableOIDCIdentities(r, s.access.Pool, p, locallyManaged)
	if err != nil {
		return httpkit.Reply{}, err
	}
	samlUsable, err := usableSAMLIdentities(r, s.access.Pool, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	hasFallback := locallyManaged && (local && localEnabled && !s.access.LocalLoginDisabled() || len(samlUsable) > 0)
	rows, err := s.access.Pool.Query(r.Context(), "SELECT id::text,issuer,email_at_link,created_at,last_login_at FROM olp.oidc_identities WHERE user_id=$1 ORDER BY created_at,id LIMIT 100", p.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []oidcIdentityItem{}
	for rows.Next() {
		var item oidcIdentityItem
		if err := rows.Scan(&item.ID, &item.Issuer, &item.EmailAtLink, &item.CreatedAt, &item.LastLoginAt); err != nil {
			return httpkit.Reply{}, err
		}
		remaining := len(usable)
		if usable[item.ID] {
			remaining--
		}
		item.CanUnlink = remaining > 0 || hasFallback
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(oidcIdentitiesResponse{
		Items:                         items,
		LinkingAvailable:              enabled,
		HasLocalPassword:              local,
		OIDCReauthenticationAvailable: len(usable) > 0,
	}), nil
}

func (s *oidcService) ensureRemainingOIDCSignIn(r *http.Request, tx pgx.Tx, p authz.Principal) error {
	var local, localEnabled, locallyManaged bool
	if err := tx.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL,COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),role_management<>'oidc' FROM olp.users WHERE id=$1", p.ID).Scan(&local, &localEnabled, &locallyManaged); err != nil {
		return err
	}
	if local && locallyManaged && localEnabled && !s.access.LocalLoginDisabled() {
		return nil
	}
	usable, err := usableOIDCIdentities(r, tx, p, locallyManaged)
	if err != nil {
		return err
	}
	// An OIDC-managed account answers to the IdP, so no other sign-in method
	// may outlive the identity through which the IdP can revoke its role.
	var samlUsable []string
	if locallyManaged {
		if samlUsable, err = usableSAMLIdentities(r, tx, p); err != nil {
			return err
		}
	}
	if len(usable) == 0 && len(samlUsable) == 0 {
		return httpkit.Fail(409, "last_sign_in_method", "Keep at least one usable sign-in method.")
	}
	return nil
}

func (s *oidcService) unlinkOIDCIdentity(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "identity_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var owner string
		if err := tx.QueryRow(r.Context(), "SELECT user_id::text FROM olp.oidc_identities WHERE id=$1", id).Scan(&owner); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if owner != p.ID {
			return httpkit.Reply{}, access.Event{}, httpkit.Forbidden()
		}
		if err := s.access.ConsumeRecent(r, tx, p, "oidc_unlink", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "DELETE FROM olp.oidc_identities WHERE id=$1", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := s.ensureRemainingOIDCSignIn(r, tx, p); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		// Removing an identity is no sign-in, so the rotated session keeps the
		// method and MFA state of the session that asked.
		strength, err := sessionStrength(r.Context(), tx, p.SessionID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		rotated, err := s.sessions.changeSignInMethod(r, tx, p.ID, strength)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		rotated.Status, rotated.Body = 204, nil
		return rotated, access.Event{Action: "oidc.unlink", Resource: "oidc_identity", ID: id}, nil
	})
}

// Only called after signature, claims, flow and configuration verification.
// Administrative active status remains independent of external authorization.
func syncOIDCAuthority(r *http.Request, tx pgx.Tx, userID, mapped string) (changed, allowed bool, err error) {
	return syncFederatedAuthority(r, tx, userID, mapped, "oidc")
}

func oidcFailureRedirect(flow oidcFlow, err error) httpkit.Reply {
	reason := "provider"
	var problem *httpkit.Problem
	if errors.As(err, &problem) {
		switch problem.Code {
		case "oidc_authorization_denied":
			reason = "cancelled"
		case "oidc_flow_invalid", "oidc_session_changed", "oidc_recent_auth_required":
			reason = "expired"
		case "oidc_provisioning_denied", "account_disabled":
			reason = "denied"
		case "oidc_identity_linked", "oidc_link_required", "oidc_identity_mismatch":
			reason = "link"
		}
	}
	destination := "/login"
	query := url.Values{"oidc_error": {reason}}
	if flow.Kind == "link" || flow.Kind == "reauthenticate" {
		destination = "/settings/profile"
	} else if flow.ReturnTo != "" && safeReturn(flow.ReturnTo) {
		query.Set("return_to", flow.ReturnTo)
	}
	reply := httpkit.Reply{Status: 303, Location: destination + "?" + query.Encode()}
	if flow.ID != "" {
		reply.Cookies = []*http.Cookie{access.ClearCookie("__Host-olp_oidc_login_" + flow.ID)}
	}
	return reply
}
