package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
)

func samlSignInRole(c samlConfiguration, managed, assigned string, data []byte) string {
	var f samlFacts
	if !c.Enabled || len(data) == 0 || json.Unmarshal(data, &f) != nil || f.EmailAttribute != c.EmailAttribute || f.GroupsAttribute != c.GroupsAttribute {
		return ""
	}
	if managed != "saml" {
		return assigned
	}
	return samlMappedRole(c, f.Email, f.Groups)
}

func usableSAMLOwner(ctx context.Context, q database.Queryer) (bool, error) {
	c, err := loadSAML(ctx, q)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !c.Enabled {
		return false, nil
	}
	if _, err = validateSAML(c); err != nil {
		return false, nil
	}
	rows, err := q.Query(ctx, "SELECT u.role_management,i.role_claims FROM olp.users u JOIN olp.saml_identities i ON i.user_id=u.id WHERE u.active AND u.oidc_authorized AND u.role='owner' AND u.access_scope='global' AND i.issuer=$1", c.EntityID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var managed string
		var raw []byte
		if err = rows.Scan(&managed, &raw); err != nil {
			return false, err
		}
		if samlSignInRole(c, managed, "owner", raw) == "owner" {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Server) samlIdentities(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	rows, err := s.access.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'issuer',issuer,'subject',subject,'email_at_link',email_at_link,'created_at',created_at,'last_login_at',last_login_at) FROM olp.saml_identities WHERE user_id=$1 ORDER BY created_at,id`, p.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	if err != nil {
		return httpkit.Reply{}, err
	}
	usable, err := usableSAMLIdentities(r, s.access.Pool, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	c, err := loadSAML(r.Context(), s.access.Pool)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(map[string]any{"items": items, "linking_available": err == nil && c.Enabled, "reauthentication_available": len(usable) > 0}), nil
}

func (s *Server) unlinkSAML(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "identity_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err = s.access.ConsumeRecent(r, tx, p, "saml_unlink", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result, err := tx.Exec(r.Context(), "DELETE FROM olp.saml_identities WHERE id=$1 AND user_id=$2", id, p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if result.RowsAffected() != 1 {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(404, "not_found", "The linked identity was not found.")
		}
		var local bool
		if err = tx.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL AND $2 AND COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true) FROM olp.users WHERE id=$1", p.ID, !s.access.LocalLoginDisabled()).Scan(&local); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var management string
		if err = tx.QueryRow(r.Context(), "SELECT role_management FROM olp.users WHERE id=$1", p.ID).Scan(&management); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		// A SAML-managed account answers to the IdP, so neither its password nor an
		// OIDC identity is a fallback that would let it keep a mapped role once the
		// IdP can no longer revoke it.
		if !local || management == "saml" {
			var usable map[string]bool
			if management != "saml" {
				var e error
				if usable, e = usableOIDCIdentities(r, tx, p, management != "oidc"); e != nil {
					return httpkit.Reply{}, access.Event{}, e
				}
			}
			if len(usable) == 0 {
				c, e := loadSAML(r.Context(), tx)
				if e != nil && !errors.Is(e, pgx.ErrNoRows) {
					return httpkit.Reply{}, access.Event{}, e
				}
				rows, e := tx.Query(r.Context(), "SELECT role_claims FROM olp.saml_identities WHERE user_id=$1 AND issuer=$2", p.ID, c.EntityID)
				if e != nil {
					return httpkit.Reply{}, access.Event{}, e
				}
				found := false
				for rows.Next() {
					var raw []byte
					if e = rows.Scan(&raw); e != nil {
						rows.Close()
						return httpkit.Reply{}, access.Event{}, e
					}
					if samlSignInRole(c, management, p.Role, raw) != "" {
						found = true
					}
				}
				e = rows.Err()
				rows.Close()
				if e != nil {
					return httpkit.Reply{}, access.Event{}, e
				}
				if !found {
					return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "last_sign_in_method", "Keep at least one usable sign-in method.")
				}
			}
		}
		if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET etag=$2,updated_at=now() WHERE id=$1", p.ID, ids.New()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		// Removing an identity is no sign-in, so the rotated session keeps the
		// method and MFA state of the session that asked.
		strength, err := sessionStrength(r, tx, p.SessionID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", p.ID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		session, err := s.newSession(r, tx, p.ID, strength)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		session.Status = 204
		session.Body = nil
		return session, access.Event{Action: "saml.unlink", Resource: "saml_identity", ID: id}, nil
	})
}

func usableSAMLIdentities(r *http.Request, q database.Queryer, p authz.Principal) ([]string, error) {
	c, err := loadSAML(r.Context(), q)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !c.Enabled {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = validateSAML(c); err != nil {
		return nil, nil
	}
	var management string
	if err = q.QueryRow(r.Context(), "SELECT role_management FROM olp.users WHERE id=$1", p.ID).Scan(&management); err != nil {
		return nil, err
	}
	rows, err := q.Query(r.Context(), "SELECT id::text,role_claims FROM olp.saml_identities WHERE user_id=$1 AND issuer=$2", p.ID, c.EntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if samlSignInRole(c, management, p.Role, raw) != "" {
			result = append(result, id)
		}
	}
	return result, rows.Err()
}
