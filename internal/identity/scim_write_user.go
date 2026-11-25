package identity

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

type parsedSCIMUser struct {
	address   string
	external  string
	display   string
	active    bool
	extension map[string]any
	stored    scim.Document
}

func parseSCIMUserEmails(raw any) ([]any, error) {
	items, ok := raw.([]any)
	if !ok || len(items) > 8 {
		return nil, scim.Invalid("emails must contain at most eight values.")
	}
	emails := []any{}
	primary := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, scim.Invalid("Email values must be objects.")
		}
		value, e := boundedSCIMText(item["value"], 320, true)
		if e != nil {
			return nil, e
		}
		value, e = email(value)
		if e != nil {
			return nil, scim.Invalid("An email value is invalid.")
		}
		entry := map[string]any{"value": value}
		typ, e := boundedSCIMText(item["type"], 32, false)
		if e != nil {
			return nil, e
		}
		if typ != "" {
			entry["type"] = typ
		}
		if flag, exists := item["primary"]; exists && flag != nil {
			b, ok := flag.(bool)
			if !ok || b && primary {
				return nil, scim.Invalid("At most one email may be primary.")
			}
			entry["primary"] = b
			primary = primary || b
		}
		emails = append(emails, entry)
	}
	return emails, nil
}

func parseSCIMUserDocument(d scim.Document) (parsedSCIMUser, error) {
	if err := scim.Schema(d, scim.UserSchema); err != nil {
		return parsedSCIMUser{}, err
	}
	if _, ok := d["password"]; ok {
		return parsedSCIMUser{}, scim.Fail(400, "mutability", "SCIM cannot provision local passwords.")
	}
	raw, err := boundedSCIMText(d["userName"], 320, true)
	if err != nil {
		return parsedSCIMUser{}, err
	}
	address, err := email(raw)
	if err != nil {
		return parsedSCIMUser{}, scim.Invalid("userName must be an email address.")
	}
	external, err := boundedSCIMText(d["externalId"], 255, false)
	if err != nil {
		return parsedSCIMUser{}, err
	}
	display, err := boundedSCIMText(d["displayName"], 100, false)
	if err != nil {
		return parsedSCIMUser{}, err
	}
	name := map[string]any{}
	if raw := d["name"]; raw != nil {
		fields, ok := raw.(map[string]any)
		if !ok {
			return parsedSCIMUser{}, scim.Invalid("name must be an object.")
		}
		for _, key := range scim.NameSubattributes() {
			if value, exists := fields[key]; exists {
				v, e := boundedSCIMText(value, 100, false)
				if e != nil {
					return parsedSCIMUser{}, e
				}
				if v != "" {
					name[key] = v
				}
			}
		}
	}
	if display == "" {
		display = scim.String(name, "formatted")
	}
	if display == "" {
		display = string([]rune(address)[:min(100, len([]rune(address)))])
	}
	active := true
	if raw, exists := d["active"]; exists && raw != nil {
		var ok bool
		active, ok = raw.(bool)
		if !ok {
			return parsedSCIMUser{}, scim.Invalid("active must be boolean.")
		}
	}
	extension, err := scimExtension(d, scim.UserExtension, true)
	if err != nil {
		return parsedSCIMUser{}, err
	}
	stored := scim.Document{"schemas": []string{scim.UserSchema, scim.UserExtension}, "userName": address, "displayName": display, "active": active, scim.UserExtension: extension}
	if external != "" {
		stored["externalId"] = external
	}
	if len(name) > 0 {
		stored["name"] = name
	}
	if raw := d["emails"]; raw != nil {
		emails, err := parseSCIMUserEmails(raw)
		if err != nil {
			return parsedSCIMUser{}, err
		}
		stored["emails"] = emails
	}
	return parsedSCIMUser{address: address, external: external, display: display, active: active, extension: extension, stored: stored}, nil
}

func provisionSCIMUser(r *http.Request, tx pgx.Tx, id string, user parsedSCIMUser) (string, error) {
	// Re-enrolling the same deleted external identity preserves its stable ID,
	// but never adopts an existing local or live account by email.
	var existing, management string
	var deleted bool
	var oldExternal *string
	e := pgx.ErrNoRows
	if user.external != "" {
		// The directory may re-enroll a deleted identity under a new userName,
		// so its externalId finds it first.
		e = tx.QueryRow(r.Context(), "SELECT u.id::text,u.role_management,su.deleted_at IS NOT NULL,su.document->>'externalId' FROM olp.scim_users su JOIN olp.users u ON u.id=su.user_id WHERE su.document->>'externalId'=$1 FOR UPDATE OF u", user.external).Scan(&existing, &management, &deleted, &oldExternal)
		if e == nil {
			var taken bool
			if e = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1 AND id<>$2)", user.address, existing).Scan(&taken); e != nil {
				return "", e
			}
			if taken {
				return "", scim.Fail(409, "uniqueness", "Another account already uses this userName.")
			}
		}
	}
	if errors.Is(e, pgx.ErrNoRows) {
		e = tx.QueryRow(r.Context(), "SELECT u.id::text,u.role_management,COALESCE(su.deleted_at IS NOT NULL,false),su.document->>'externalId' FROM olp.users u LEFT JOIN olp.scim_users su ON su.user_id=u.id WHERE u.email=$1 FOR UPDATE OF u", user.address).Scan(&existing, &management, &deleted, &oldExternal)
	}
	if e == nil {
		old := ""
		if oldExternal != nil {
			old = *oldExternal
		}
		if management != "provisioned" || !deleted || old != user.external {
			return "", scim.Fail(409, "uniqueness", "Another account already uses this userName.")
		}
		return existing, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return "", e
	}
	if _, e = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,active,access_scope,role_management,etag) VALUES($1,$2,$3,$4,$5,$6,'provisioned',$7)", id, user.address, user.display, user.extension["role"], user.active, user.extension["accessScope"], ids.New()); e != nil {
		return "", e
	}
	if _, e = tx.Exec(r.Context(), "INSERT INTO olp.provisioned_users(source,external_id,user_id) VALUES('scim',$1::uuid::text,$1::uuid)", id); e != nil {
		return "", e
	}
	return id, nil
}

func (s *Server) writeSCIMUser(r *http.Request, tx pgx.Tx, id string, d scim.Document, method string) (string, error) {
	if method != "POST" {
		var management string
		if err := tx.QueryRow(r.Context(), "SELECT role_management FROM olp.users WHERE id=$1 FOR UPDATE", id).Scan(&management); err != nil {
			return "", err
		}
		if management != "provisioned" {
			return "", scim.Fail(409, "mutability", "This identity is now managed locally.")
		}
	}
	if method == "DELETE" {
		if _, err := tx.Exec(r.Context(), "UPDATE olp.scim_users SET deleted_at=now() WHERE user_id=$1", id); err != nil {
			return "", err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET active=false,etag=$2,updated_at=now() WHERE id=$1", id, ids.New()); err != nil {
			return "", err
		}
		_, err := tx.Exec(r.Context(), "DELETE FROM olp.scim_group_members WHERE user_id=$1", id)
		return id, err
	}
	user, err := parseSCIMUserDocument(d)
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(user.stored)
	if method == "POST" {
		id, err = provisionSCIMUser(r, tx, id, user)
		if err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.scim_users(user_id,document) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET document=excluded.document,deleted_at=NULL", id, data); err != nil {
		return "", err
	}
	_, err = tx.Exec(r.Context(), "UPDATE olp.users SET email=$2,display_name=$3,active=$4,updated_at=now(),etag=$5 WHERE id=$1", id, user.address, user.display, user.active, ids.New())
	return id, err
}
