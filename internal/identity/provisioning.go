package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
)

type provisionInput struct {
	source      string
	externalID  string
	email       string
	displayName string
	role        string
	active      bool
}

func provisioningID(r *http.Request, name, field string, max int) (string, error) {
	value := r.PathValue(name)
	if name == "source" && value == "scim" {
		return "", problem.Fail(409, "managed_provisioning_source", "Use the SCIM API to manage its identities.")
	}
	if value == "" || len(value) > max || strings.ContainsAny(value, "/\r\n") || strings.ContainsFunc(value, unicode.IsControl) {
		return "", problem.Invalid(field, "Invalid provisioning identifier.")
	}
	return value, nil
}

func parseProvisionInput(r *http.Request) (provisionInput, error) {
	var raw struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		Active      *bool  `json:"active"`
	}
	if err := httpkit.Decode(r, &raw); err != nil {
		return provisionInput{}, err
	}
	source, err := provisioningID(r, "source", "source", 100)
	if err != nil {
		return provisionInput{}, err
	}
	external, err := provisioningID(r, "external_id", "external_id", 255)
	if err != nil {
		return provisionInput{}, err
	}
	address, err := email(raw.Email)
	if err != nil {
		return provisionInput{}, err
	}
	if err := textfield.Validate("display_name", raw.DisplayName, 100); err != nil {
		return provisionInput{}, err
	}
	if !authz.ValidRole(raw.Role) {
		return provisionInput{}, problem.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	if raw.Active == nil {
		return provisionInput{}, problem.Invalid("active", "Provide the active state.")
	}
	return provisionInput{
		source:      source,
		externalID:  external,
		email:       address,
		displayName: strings.TrimSpace(raw.DisplayName),
		role:        raw.Role,
		active:      *raw.Active,
	}, nil
}

func lookupProvisionedUser(ctx context.Context, tx pgx.Tx, source, external string) (string, bool, error) {
	var userID, management string
	err := tx.QueryRow(ctx, "SELECT pu.user_id::text,u.role_management FROM olp.provisioned_users pu JOIN olp.users u ON u.id=pu.user_id WHERE pu.source=$1 AND pu.external_id=$2 FOR UPDATE", source, external).Scan(&userID, &management)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if management != "provisioned" {
		return "", false, problem.Fail(409, "provisioning_ownership_changed", "This identity is now managed locally and cannot be reconciled.")
	}
	return userID, true, nil
}

func ensureProvisionEmailAvailable(ctx context.Context, tx pgx.Tx, address, excludeUserID string) error {
	var taken bool
	var err error
	if excludeUserID != "" {
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1 AND id<>$2)", address, excludeUserID).Scan(&taken)
	} else {
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&taken)
	}
	if err != nil {
		return err
	}
	if taken {
		return problem.Fail(409, "email_unavailable", "Another account already uses this email address.")
	}
	return nil
}

func upsertProvisionedUser(ctx context.Context, tx pgx.Tx, input provisionInput, existingID string, mapped bool) (string, error) {
	if !mapped {
		userID := ids.New()
		if _, err := tx.Exec(ctx, "INSERT INTO olp.users(id,email,display_name,role,active,etag,role_management) VALUES($1,$2,$3,$4,$5,$6,'provisioned')", userID, input.email, input.displayName, input.role, input.active, ids.New()); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO olp.provisioned_users(source,external_id,user_id) VALUES($1,$2,$3)", input.source, input.externalID, userID); err != nil {
			return "", err
		}
		return userID, nil
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.users SET email=$1,display_name=$2,role=$3,active=$4,etag=$5,updated_at=now() WHERE id=$6", input.email, input.displayName, input.role, input.active, ids.New(), existingID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.provisioned_users SET updated_at=now() WHERE source=$1 AND external_id=$2", input.source, input.externalID); err != nil {
		return "", err
	}
	return existingID, nil
}

func (s *accountService) provisionUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input, err := parseProvisionInput(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return s.applyProvisionUser(r, tx, p, input)
	})
}

func (s *accountService) applyProvisionUser(r *http.Request, tx pgx.Tx, p authz.Principal, input provisionInput) (httpkit.Reply, access.Event, error) {
	existingID, mapped, err := lookupProvisionedUser(r.Context(), tx, input.source, input.externalID)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := ensureProvisionEmailAvailable(r.Context(), tx, input.email, existingID); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	userID, err := upsertProvisionedUser(r.Context(), tx, input, existingID, mapped)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if !input.active || input.role != "owner" {
		if err := retireIssuedInvitations(r.Context(), tx, r, userID, p.Actor(), p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
	}
	if err := EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	u, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", userID))
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Detail(u, u.ETag), access.Event{Action: "user.provision", Resource: "user", ID: userID}, nil
}

func (s *accountService) deprovisionUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	source, err := provisioningID(r, "source", "source", 100)
	if err != nil {
		return httpkit.Reply{}, err
	}
	external, err := provisioningID(r, "external_id", "external_id", 255)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		userID, mapped, err := lookupProvisionedUser(r.Context(), tx, source, external)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !mapped {
			return httpkit.Reply{}, access.Event{}, pgx.ErrNoRows
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET active=false,etag=$1,updated_at=now() WHERE id=$2", ids.New(), userID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := retireIssuedInvitations(r.Context(), tx, r, userID, p.Actor(), p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 204}, access.Event{Action: "user.deprovision", Resource: "user", ID: userID}, nil
	})
}
