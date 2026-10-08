package access

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
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

func (s *Server) usableSAMLOwner(r *http.Request, q Queryer) (bool, error) {
	c, err := loadSAML(r, q)
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
	rows, err := q.Query(r.Context(), "SELECT u.role_management,i.role_claims FROM olp.users u JOIN olp.saml_identities i ON i.user_id=u.id WHERE u.active AND u.oidc_authorized AND u.role='owner' AND u.access_scope='global' AND i.issuer=$1", c.EntityID)
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

func (s *Server) samlIdentities(r *http.Request, p Principal) (Reply, error) {
	rows, err := s.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'issuer',issuer,'subject',subject,'email_at_link',email_at_link,'created_at',created_at,'last_login_at',last_login_at) FROM olp.saml_identities WHERE user_id=$1 ORDER BY created_at,id`, p.ID)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	if err != nil {
		return Reply{}, err
	}
	usable, err := usableSAMLIdentities(r, s.Pool, p)
	if err != nil {
		return Reply{}, err
	}
	c, err := loadSAML(r, s.Pool)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	return OK(map[string]any{"items": items, "linking_available": err == nil && c.Enabled, "reauthentication_available": len(usable) > 0}), nil
}

func (s *Server) unlinkSAML(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "identity_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = s.ConsumeRecent(r, tx, p, "saml_unlink", id); err != nil {
		return Reply{}, err
	}
	result, err := tx.Exec(r.Context(), "DELETE FROM olp.saml_identities WHERE id=$1 AND user_id=$2", id, p.ID)
	if err != nil {
		return Reply{}, err
	}
	if result.RowsAffected() != 1 {
		return Reply{}, Fail(404, "not_found", "The linked identity was not found.")
	}
	var local bool
	if err = tx.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL AND $2 AND COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true) FROM olp.users WHERE id=$1", p.ID, !s.LocalLoginDisabled).Scan(&local); err != nil {
		return Reply{}, err
	}
	var management string
	if err = tx.QueryRow(r.Context(), "SELECT role_management FROM olp.users WHERE id=$1", p.ID).Scan(&management); err != nil {
		return Reply{}, err
	}
	if !local {
		usable, e := usableOIDCIdentities(r, tx, p, management != "oidc")
		if e != nil {
			return Reply{}, e
		}
		if len(usable) == 0 {
			c, e := loadSAML(r, tx)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return Reply{}, e
			}
			rows, e := tx.Query(r.Context(), "SELECT role_claims FROM olp.saml_identities WHERE user_id=$1 AND issuer=$2", p.ID, c.EntityID)
			if e != nil {
				return Reply{}, e
			}
			found := false
			for rows.Next() {
				var raw []byte
				if e = rows.Scan(&raw); e != nil {
					rows.Close()
					return Reply{}, e
				}
				if samlSignInRole(c, management, p.Role, raw) != "" {
					found = true
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return Reply{}, e
			}
			if !found {
				return Reply{}, Fail(409, "last_sign_in_method", "Keep at least one usable sign-in method.")
			}
		}
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET etag=$2,updated_at=now() WHERE id=$1", p.ID, NewID()); err != nil {
		return Reply{}, err
	}
	// Removing an identity is no sign-in, so the rotated session keeps the
	// method and MFA state of the session that asked.
	strength, err := sessionStrength(r, tx, p.SessionID)
	if err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", p.ID); err != nil {
		return Reply{}, err
	}
	session, err := s.newSession(r, tx, p.ID, strength)
	if err != nil {
		return Reply{}, err
	}
	session.Status = 204
	session.Body = nil
	if err = Audit(r.Context(), tx, r, p.Actor(), "saml.unlink", "saml_identity", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, session)
}

func usableSAMLIdentities(r *http.Request, q Queryer, p Principal) ([]string, error) {
	c, err := loadSAML(r, q)
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
