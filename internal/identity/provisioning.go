package identity

import (
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

func provisioningID(r *http.Request, name, field string, max int) (string, error) {
	value := r.PathValue(name)
	if name == "source" && value == "scim" {
		return "", httpkit.Fail(409, "managed_provisioning_source", "Use the SCIM API to manage its identities.")
	}
	if value == "" || len(value) > max || strings.ContainsAny(value, "/\r\n") || strings.ContainsFunc(value, unicode.IsControl) {
		return "", httpkit.Invalid(field, "Invalid provisioning identifier.")
	}
	return value, nil
}

func (s *Server) provisionUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		Active      *bool  `json:"active"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	source, err := provisioningID(r, "source", "source", 100)
	if err != nil {
		return httpkit.Reply{}, err
	}
	external, err := provisioningID(r, "external_id", "external_id", 255)
	if err != nil {
		return httpkit.Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = httpkit.ValidText("display_name", input.DisplayName, 100); err != nil {
		return httpkit.Reply{}, err
	}
	if !authz.ValidRole(input.Role) {
		return httpkit.Reply{}, httpkit.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	if input.Active == nil {
		return httpkit.Reply{}, httpkit.Invalid("active", "Provide the active state.")
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var userID, management string
	err = tx.QueryRow(r.Context(), "SELECT pu.user_id::text,u.role_management FROM olp.provisioned_users pu JOIN olp.users u ON u.id=pu.user_id WHERE pu.source=$1 AND pu.external_id=$2 FOR UPDATE", source, external).Scan(&userID, &management)
	mapped := !errors.Is(err, pgx.ErrNoRows)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, err
	}
	if mapped && management != "provisioned" {
		return httpkit.Reply{}, httpkit.Fail(409, "provisioning_ownership_changed", "This identity is now managed locally and cannot be reconciled.")
	}
	var taken bool
	if mapped {
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1 AND id<>$2)", address, userID).Scan(&taken)
	} else {
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&taken)
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	if taken {
		return httpkit.Reply{}, httpkit.Fail(409, "email_unavailable", "Another account already uses this email address.")
	}
	if !mapped {
		userID = httpkit.NewID()
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,active,etag,role_management) VALUES($1,$2,$3,$4,$5,$6,'provisioned')", userID, address, strings.TrimSpace(input.DisplayName), input.Role, *input.Active, httpkit.NewID()); err != nil {
			return httpkit.Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.provisioned_users(source,external_id,user_id) VALUES($1,$2,$3)", source, external, userID); err != nil {
			return httpkit.Reply{}, err
		}
	} else {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET email=$1,display_name=$2,role=$3,active=$4,etag=$5,updated_at=now() WHERE id=$6", address, strings.TrimSpace(input.DisplayName), input.Role, *input.Active, httpkit.NewID(), userID); err != nil {
			return httpkit.Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "UPDATE olp.provisioned_users SET updated_at=now() WHERE source=$1 AND external_id=$2", source, external); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return httpkit.Reply{}, err
	}
	if !*input.Active || input.Role != "owner" {
		if err = retireIssuedInvitations(r, tx, userID, p.Actor(), p.UserID()); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "user.provision", "user", userID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	u, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", userID))
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(u, u.ETag), tx.Commit(r.Context())
}

func (s *Server) deprovisionUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	source, err := provisioningID(r, "source", "source", 100)
	if err != nil {
		return httpkit.Reply{}, err
	}
	external, err := provisioningID(r, "external_id", "external_id", 255)
	if err != nil {
		return httpkit.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var userID, management string
	if err = tx.QueryRow(r.Context(), "SELECT pu.user_id::text,u.role_management FROM olp.provisioned_users pu JOIN olp.users u ON u.id=pu.user_id WHERE pu.source=$1 AND pu.external_id=$2 FOR UPDATE", source, external).Scan(&userID, &management); err != nil {
		return httpkit.Reply{}, err
	}
	if management != "provisioned" {
		return httpkit.Reply{}, httpkit.Fail(409, "provisioning_ownership_changed", "This identity is now managed locally and cannot be reconciled.")
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET active=false,etag=$1,updated_at=now() WHERE id=$2", httpkit.NewID(), userID); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return httpkit.Reply{}, err
	}
	if err = retireIssuedInvitations(r, tx, userID, p.Actor(), p.UserID()); err != nil {
		return httpkit.Reply{}, err
	}
	if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "user.deprovision", "user", userID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Reply{Status: 204}, tx.Commit(r.Context())
}
