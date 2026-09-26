package access

import (
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

func provisioningID(r *http.Request, name, field string, max int) (string, error) {
	value := r.PathValue(name)
	if value == "" || len(value) > max || strings.ContainsAny(value, "/\r\n") || strings.ContainsFunc(value, unicode.IsControl) {
		return "", Invalid(field, "Invalid provisioning identifier.")
	}
	return value, nil
}

func (s *Server) provisionUser(r *http.Request) (Reply, error) {
	var input struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		Active      *bool  `json:"active"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	source, err := provisioningID(r, "source", "source", 100)
	if err != nil {
		return Reply{}, err
	}
	external, err := provisioningID(r, "external_id", "external_id", 255)
	if err != nil {
		return Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return Reply{}, err
	}
	if err = ValidText("display_name", input.DisplayName, 100); err != nil {
		return Reply{}, err
	}
	if !validRole(input.Role) {
		return Reply{}, Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	if input.Active == nil {
		return Reply{}, Invalid("active", "Provide the active state.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "access")
	if err != nil {
		return Reply{}, err
	}
	var userID, management string
	err = tx.QueryRow(r.Context(), "SELECT pu.user_id::text,u.role_management FROM olp.provisioned_users pu JOIN olp.users u ON u.id=pu.user_id WHERE pu.source=$1 AND pu.external_id=$2 FOR UPDATE", source, external).Scan(&userID, &management)
	mapped := !errors.Is(err, pgx.ErrNoRows)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	if mapped && management != "provisioned" {
		return Reply{}, Fail(409, "provisioning_ownership_changed", "This identity is now managed locally and cannot be reconciled.")
	}
	var taken bool
	if mapped {
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1 AND id<>$2)", address, userID).Scan(&taken)
	} else {
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&taken)
	}
	if err != nil {
		return Reply{}, err
	}
	if taken {
		return Reply{}, Fail(409, "email_unavailable", "Another account already uses this email address.")
	}
	if !mapped {
		userID = NewID()
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,active,etag,role_management) VALUES($1,$2,$3,$4,$5,$6,'provisioned')", userID, address, strings.TrimSpace(input.DisplayName), input.Role, *input.Active, NewID()); err != nil {
			return Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.provisioned_users(source,external_id,user_id) VALUES($1,$2,$3)", source, external, userID); err != nil {
			return Reply{}, err
		}
	} else {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET email=$1,display_name=$2,role=$3,active=$4,etag=$5,updated_at=now() WHERE id=$6", address, strings.TrimSpace(input.DisplayName), input.Role, *input.Active, NewID(), userID); err != nil {
			return Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "UPDATE olp.provisioned_users SET updated_at=now() WHERE source=$1 AND external_id=$2", source, external); err != nil {
			return Reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return Reply{}, err
	}
	if !*input.Active {
		if err = retireIssuedInvitations(r, tx, userID, p.ID, p.UserID()); err != nil {
			return Reply{}, err
		}
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "user.provision", "user", userID, "success"); err != nil {
		return Reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp.users u WHERE id=$1", userID))
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(u, u.ETag))
}

func (s *Server) deprovisionUser(r *http.Request) (Reply, error) {
	source, err := provisioningID(r, "source", "source", 100)
	if err != nil {
		return Reply{}, err
	}
	external, err := provisioningID(r, "external_id", "external_id", 255)
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "access")
	if err != nil {
		return Reply{}, err
	}
	var userID, management string
	if err = tx.QueryRow(r.Context(), "SELECT pu.user_id::text,u.role_management FROM olp.provisioned_users pu JOIN olp.users u ON u.id=pu.user_id WHERE pu.source=$1 AND pu.external_id=$2 FOR UPDATE", source, external).Scan(&userID, &management); err != nil {
		return Reply{}, err
	}
	if management != "provisioned" {
		return Reply{}, Fail(409, "provisioning_ownership_changed", "This identity is now managed locally and cannot be reconciled.")
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET active=false,etag=$1,updated_at=now() WHERE id=$2", NewID(), userID); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return Reply{}, err
	}
	if err = retireIssuedInvitations(r, tx, userID, p.ID, p.UserID()); err != nil {
		return Reply{}, err
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "user.deprovision", "user", userID, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Reply{Status: 204})
}
