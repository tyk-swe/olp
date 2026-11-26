package identity

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

type scimEmailEntry struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary *bool  `json:"primary,omitempty"`
}

type scimUserName struct {
	Formatted       string `json:"formatted,omitempty"`
	FamilyName      string `json:"familyName,omitempty"`
	GivenName       string `json:"givenName,omitempty"`
	MiddleName      string `json:"middleName,omitempty"`
	HonorificPrefix string `json:"honorificPrefix,omitempty"`
	HonorificSuffix string `json:"honorificSuffix,omitempty"`
}

type scimStoredUser struct {
	Schemas     []string          `json:"schemas"`
	UserName    string            `json:"userName"`
	DisplayName string            `json:"displayName"`
	Active      bool              `json:"active"`
	ExternalID  string            `json:"externalId,omitempty"`
	Name        *scimUserName     `json:"name,omitempty"`
	Emails      *[]scimEmailEntry `json:"emails,omitempty"`
	Extension   scimUserExtension `json:"urn:openllmproxy:params:scim:schemas:extension:2.0:User"`
}

type parsedSCIMUser struct {
	address   string
	external  string
	display   string
	active    bool
	extension scimUserExtension
	stored    scimStoredUser
}

func parseSCIMUserEmails(raw any) ([]scimEmailEntry, error) {
	items, ok := raw.([]any)
	if !ok || len(items) > 8 {
		return nil, scim.Invalid("emails must contain at most eight values.")
	}
	emails := make([]scimEmailEntry, 0, len(items))
	primary := false
	for _, entryRaw := range items {
		entry, isPrimary, err := parseSCIMEmailEntry(entryRaw, primary)
		if err != nil {
			return nil, err
		}
		primary = primary || isPrimary
		emails = append(emails, entry)
	}
	return emails, nil
}

func parseSCIMEmailEntry(raw any, seenPrimary bool) (scimEmailEntry, bool, error) {
	item, ok := raw.(map[string]any)
	if !ok {
		return scimEmailEntry{}, false, scim.Invalid("Email values must be objects.")
	}
	value, err := boundedSCIMText(item["value"], 320, true)
	if err != nil {
		return scimEmailEntry{}, false, err
	}
	normalized, err := email(value)
	if err != nil {
		return scimEmailEntry{}, false, scim.Invalid("An email value is invalid.")
	}
	typ, err := boundedSCIMText(item["type"], 32, false)
	if err != nil {
		return scimEmailEntry{}, false, err
	}
	entry := scimEmailEntry{Value: normalized, Type: typ}
	flag, exists := item["primary"]
	if !exists || flag == nil {
		return entry, false, nil
	}
	b, ok := flag.(bool)
	if !ok || b && seenPrimary {
		return scimEmailEntry{}, false, scim.Invalid("At most one email may be primary.")
	}
	entry.Primary = &b
	return entry, b, nil
}

func parseSCIMUserName(raw any) (*scimUserName, error) {
	if raw == nil {
		return nil, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, scim.Invalid("name must be an object.")
	}
	var name scimUserName
	assigned := false
	targets := map[string]*string{
		"formatted":       &name.Formatted,
		"familyName":      &name.FamilyName,
		"givenName":       &name.GivenName,
		"middleName":      &name.MiddleName,
		"honorificPrefix": &name.HonorificPrefix,
		"honorificSuffix": &name.HonorificSuffix,
	}
	for _, key := range scim.NameSubattributes() {
		value, exists := fields[key]
		if !exists {
			continue
		}
		text, err := boundedSCIMText(value, 100, false)
		if err != nil {
			return nil, err
		}
		if text != "" {
			*targets[key] = text
			assigned = true
		}
	}
	if !assigned {
		return nil, nil
	}
	return &name, nil
}

func parseSCIMUserActive(d scim.Document) (bool, error) {
	raw, exists := d["active"]
	if !exists || raw == nil {
		return true, nil
	}
	active, ok := raw.(bool)
	if !ok {
		return false, scim.Invalid("active must be boolean.")
	}
	return active, nil
}

func resolveSCIMDisplayName(display string, name *scimUserName, address string) string {
	if display != "" {
		return display
	}
	if name != nil && name.Formatted != "" {
		return name.Formatted
	}
	runes := []rune(address)
	return string(runes[:min(100, len(runes))])
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
	name, err := parseSCIMUserName(d["name"])
	if err != nil {
		return parsedSCIMUser{}, err
	}
	resolvedDisplay := resolveSCIMDisplayName(display, name, address)
	active, err := parseSCIMUserActive(d)
	if err != nil {
		return parsedSCIMUser{}, err
	}
	extension, err := scimExtension(d, scim.UserExtension, true)
	if err != nil {
		return parsedSCIMUser{}, err
	}
	var emailsPtr *[]scimEmailEntry
	if rawEmails := d["emails"]; rawEmails != nil {
		emails, emailsErr := parseSCIMUserEmails(rawEmails)
		if emailsErr != nil {
			return parsedSCIMUser{}, emailsErr
		}
		emailsPtr = &emails
	}
	stored := scimStoredUser{
		Schemas:     []string{scim.UserSchema, scim.UserExtension},
		UserName:    address,
		DisplayName: resolvedDisplay,
		Active:      active,
		ExternalID:  external,
		Name:        name,
		Emails:      emailsPtr,
		Extension:   extension,
	}
	return parsedSCIMUser{
		address:   address,
		external:  external,
		display:   resolvedDisplay,
		active:    active,
		extension: extension,
		stored:    stored,
	}, nil
}

func findReenrollableSCIMUser(ctx context.Context, tx pgx.Tx, user parsedSCIMUser) (string, bool, error) {
	var existing, management string
	var deleted bool
	var oldExternal *string
	err := pgx.ErrNoRows
	if user.external != "" {
		// The directory may re-enroll a deleted identity under a new userName,
		// so its externalId finds it first.
		err = tx.QueryRow(ctx, "SELECT u.id::text,u.role_management,su.deleted_at IS NOT NULL,su.document->>'externalId' FROM olp.scim_users su JOIN olp.users u ON u.id=su.user_id WHERE su.document->>'externalId'=$1 FOR UPDATE OF u", user.external).Scan(&existing, &management, &deleted, &oldExternal)
		if err == nil {
			var taken bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1 AND id<>$2)", user.address, existing).Scan(&taken); err != nil {
				return "", false, err
			}
			if taken {
				return "", false, scim.Fail(409, "uniqueness", "Another account already uses this userName.")
			}
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, "SELECT u.id::text,u.role_management,COALESCE(su.deleted_at IS NOT NULL,false),su.document->>'externalId' FROM olp.users u LEFT JOIN olp.scim_users su ON su.user_id=u.id WHERE u.email=$1 FOR UPDATE OF u", user.address).Scan(&existing, &management, &deleted, &oldExternal)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	old := ""
	if oldExternal != nil {
		old = *oldExternal
	}
	if management != "provisioned" || !deleted || old != user.external {
		return "", false, scim.Fail(409, "uniqueness", "Another account already uses this userName.")
	}
	return existing, true, nil
}

func provisionSCIMUser(ctx context.Context, tx pgx.Tx, id string, user parsedSCIMUser) (string, error) {
	// Re-enrolling the same deleted external identity preserves its stable ID,
	// but never adopts an existing local or live account by email.
	existing, found, err := findReenrollableSCIMUser(ctx, tx, user)
	if err != nil {
		return "", err
	}
	if found {
		return existing, nil
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.users(id,email,display_name,role,active,access_scope,role_management,etag) VALUES($1,$2,$3,$4,$5,$6,'provisioned',$7)", id, user.address, user.display, user.extension.Role, user.active, user.extension.AccessScope, ids.New()); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.provisioned_users(source,external_id,user_id) VALUES('scim',$1::uuid::text,$1::uuid)", id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *scimService) writeSCIMUser(ctx context.Context, tx pgx.Tx, id string, d scim.Document, method string) (string, error) {
	if method != "POST" {
		var management string
		if err := tx.QueryRow(ctx, "SELECT role_management FROM olp.users WHERE id=$1 FOR UPDATE", id).Scan(&management); err != nil {
			return "", err
		}
		if management != "provisioned" {
			return "", scim.Fail(409, "mutability", "This identity is now managed locally.")
		}
	}
	if method == "DELETE" {
		if _, err := tx.Exec(ctx, "UPDATE olp.scim_users SET deleted_at=now() WHERE user_id=$1", id); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, "UPDATE olp.users SET active=false,etag=$2,updated_at=now() WHERE id=$1", id, ids.New()); err != nil {
			return "", err
		}
		_, err := tx.Exec(ctx, "DELETE FROM olp.scim_group_members WHERE user_id=$1", id)
		return id, err
	}
	user, err := parseSCIMUserDocument(d)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(user.stored)
	if err != nil {
		return "", err
	}
	targetID := id
	if method == "POST" {
		targetID, err = provisionSCIMUser(ctx, tx, id, user)
		if err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.scim_users(user_id,document) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET document=excluded.document,deleted_at=NULL", targetID, data); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, "UPDATE olp.users SET email=$2,display_name=$3,active=$4,updated_at=now(),etag=$5 WHERE id=$1", targetID, user.address, user.display, user.active, ids.New())
	return targetID, err
}
