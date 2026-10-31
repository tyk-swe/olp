package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

// importRow is one provider_billing_imports row.
type importRow struct {
	ProviderID          string
	Source              string
	CredentialID        *string
	Scope               json.RawMessage
	Enabled             bool
	ETag                string
	NextImportAt        *time.Time
	LastImportAt        *time.Time
	LastSuccessAt       *time.Time
	LastErrorCode       *string
	ConsecutiveFailures int
	ImportedFrom        *string
	ImportedThrough     *string
	SourceRevision      *string
}

var errImportMissing = access.Fail(404, "not_found", "The provider has no billing import.")

const importColumns = `provider_id::text,source,credential_id::text,scope,enabled,etag::text,
	next_import_at,last_import_at,last_success_at,last_error_code,consecutive_failures,
	imported_from::text,imported_through::text,source_revision`

// qualifiedImportColumns binds every importColumns column to a table alias,
// for queries that join another table sharing names such as etag.
func qualifiedImportColumns(alias string) string {
	parts := strings.Split(importColumns, ",")
	for i, part := range parts {
		column, cast, found := strings.Cut(strings.TrimSpace(part), "::")
		parts[i] = alias + "." + column
		if found {
			parts[i] += "::" + cast
		}
	}
	return strings.Join(parts, ",")
}

func scanImport(row pgx.Row) (*importRow, error) {
	var r importRow
	err := row.Scan(&r.ProviderID, &r.Source, &r.CredentialID, &r.Scope, &r.Enabled, &r.ETag,
		&r.NextImportAt, &r.LastImportAt, &r.LastSuccessAt, &r.LastErrorCode, &r.ConsecutiveFailures,
		&r.ImportedFrom, &r.ImportedThrough, &r.SourceRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errImportMissing
	}
	return &r, err
}

func loadImport(ctx context.Context, q access.Queryer, providerID string, lock bool) (*importRow, error) {
	query := "SELECT " + importColumns + " FROM olp.provider_billing_imports WHERE provider_id=$1::uuid"
	if lock {
		query += " FOR UPDATE"
	}
	return scanImport(q.QueryRow(ctx, query, providerID))
}

// importDocument is the resource shape GET and PUT share. The credential is
// never part of it, only whether one is stored.
func importDocument(r *importRow) map[string]any {
	return map[string]any{
		"provider_id":           r.ProviderID,
		"source":                r.Source,
		"enabled":               r.Enabled,
		"scope":                 r.Scope,
		"credential_configured": r.CredentialID != nil,
		"etag":                  r.ETag,
		"status": map[string]any{
			"next_import_at":   r.NextImportAt,
			"last_import_at":   r.LastImportAt,
			"last_success_at":  r.LastSuccessAt,
			"last_error_code":  r.LastErrorCode,
			"imported_from":    r.ImportedFrom,
			"imported_through": r.ImportedThrough,
			"source_revision":  r.SourceRevision,
		},
	}
}

func (s *Server) getImport(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.Access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	row, err := loadImport(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(importDocument(row), row.ETag), nil
}

// putImport creates or replaces the provider's billing import. A stored
// credential survives a PUT that omits it; credential:null is rejected —
// DELETE removes the import.
func (s *Server) putImport(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input struct {
		Enabled    *bool           `json:"enabled"`
		Scope      json.RawMessage `json:"scope"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := access.DecodeUnique(r, &input, 65536); err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	// The stored reply discloses the provider, so the caller must still reach
	// its project before a replay is served.
	current, err := loadProvider(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	existing, err := loadImport(r.Context(), tx, id, true)
	if err != nil && !errors.Is(err, errImportMissing) {
		return access.Reply{}, err
	}
	etag := current.ETag
	if existing != nil {
		etag = existing.ETag
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	source, err := SourceFor(current.Kind)
	if err != nil {
		return access.Reply{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	var scope json.RawMessage = json.RawMessage(`{}`)
	if existing != nil {
		scope = existing.Scope
	}
	if len(input.Scope) > 0 {
		canonical, err := ValidateScope(source, input.Scope)
		if err != nil {
			return access.Reply{}, err
		}
		scope = canonical
	} else {
		if scope, err = ValidateScope(source, scope); err != nil {
			return access.Reply{}, err
		}
	}
	credentialID := existingCredential(existing)
	credentialNull := bytes.Equal(bytes.TrimSpace(input.Credential), []byte("null"))
	var credential *string
	if len(input.Credential) > 0 && !credentialNull {
		var secret string
		if source == SourceAWS || (source == SourceGoogle && input.Credential[0] == '{') {
			secret = string(input.Credential)
		} else if err := json.Unmarshal(input.Credential, &secret); err != nil {
			return access.Reply{}, access.Invalid("credential", "credential must be a string")
		}
		credential = &secret
	}
	switch {
	case credentialNull:
		return access.Reply{}, access.Invalid("credential", "credential cannot be null; delete the billing import instead")
	case credential != nil:
		document, err := encodeCredential(source, *credential)
		if err != nil {
			return access.Reply{}, err
		}
		defer clear(document)
		newID := access.NewID()
		if err := s.Access.Keys.Store(r.Context(), tx, s.Access.Installation, newID, secrets.BillingCredential, document, nil); err != nil {
			return access.Reply{}, err
		}
		credentialID = &newID
	case credentialID == nil:
		return access.Reply{}, access.Invalid("credential", "credential is required to create a billing import")
	}
	newETag := access.NewID()
	action := "provider.billing_import.create"
	status := http.StatusCreated
	if existing == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO olp.provider_billing_imports
			(provider_id,source,credential_id,scope,enabled,etag,created_by,next_import_at)
			VALUES($1::uuid,$2,$3::uuid,$4::jsonb,$5,$6,$7::uuid,now())`,
			id, source, credentialID, scope, enabled, newETag, p.UserID())
	} else {
		action = "provider.billing_import.update"
		status = http.StatusOK
		_, err = tx.Exec(r.Context(), `UPDATE olp.provider_billing_imports
			SET source=$2,credential_id=$3::uuid,scope=$4::jsonb,enabled=$5,etag=$6,updated_at=now()
			WHERE provider_id=$1::uuid`,
			id, source, credentialID, scope, enabled, newETag)
	}
	if err != nil {
		return access.Reply{}, err
	}
	if err := syncImportRevision(r.Context(), tx, id, source, scope); err != nil {
		return access.Reply{}, err
	}
	// The row no longer references the replaced credential, so the old secret
	// can go once the pointer moved.
	if old := existingCredential(existing); credential != nil && old != nil {
		if _, err := tx.Exec(r.Context(), `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, *old, secrets.BillingCredential); err != nil {
			return access.Reply{}, err
		}
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), action, "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	row, err := loadImport(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: status, ETag: newETag, Body: importDocument(row)}
	if err := s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func existingCredential(r *importRow) *string {
	if r == nil {
		return nil
	}
	return r.CredentialID
}

// deleteImport removes the import and its sealed credential; the costs it
// already imported stay.
func (s *Server) deleteImport(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadProvider(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	existing, err := loadImport(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, existing.ETag); err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, id); err != nil {
		return access.Reply{}, err
	}
	if existing.CredentialID != nil {
		if _, err := tx.Exec(r.Context(), `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, *existing.CredentialID, secrets.BillingCredential); err != nil {
			return access.Reply{}, err
		}
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.billing_import.delete", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 204})
}

// runImport brings the next import forward to now.
func (s *Server) runImport(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadProvider(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if _, err := loadImport(r.Context(), tx, id, true); err != nil {
		return access.Reply{}, err
	}
	newETag := access.NewID()
	if _, err := tx.Exec(r.Context(), `UPDATE olp.provider_billing_imports
		SET next_import_at=now(),etag=$2,updated_at=now() WHERE provider_id=$1::uuid`, id, newETag); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.billing_import.run", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	row, err := loadImport(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusAccepted, ETag: newETag, Body: importDocument(row)}
	if err := s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

// invoicedCosts lists the stored invoice rows for one provider over a range.
func (s *Server) invoicedCosts(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.Access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	start, end, err := dayRange(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT day::text,line_item,btrim(currency),amount::text,source,fetched_at
		FROM olp.provider_invoiced_costs
		WHERE provider_id=$1::uuid AND day>=$2::date AND day<$3::date
		ORDER BY day,line_item,currency`, id, start.Format(dayFormat), end.Format(dayFormat))
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var day, lineItem, currency, amount, source string
		var fetchedAt time.Time
		if err := rows.Scan(&day, &lineItem, &currency, &amount, &source, &fetchedAt); err != nil {
			return access.Reply{}, err
		}
		items = append(items, map[string]any{
			"day": day, "line_item": lineItem, "currency": currency,
			"amount": amount, "source": source, "fetched_at": fetchedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusOK, Body: map[string]any{"items": items}}, nil
}
