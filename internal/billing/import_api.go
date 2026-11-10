package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

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
	ResumePeriod        *time.Time
	ResumeRemaining     int
}

var errImportMissing = access.Fail(404, "not_found", "The provider has no billing import.")

const importColumns = `provider_id::text,source,credential_id::text,scope,enabled,etag::text,
	next_import_at,last_import_at,last_success_at,last_error_code,consecutive_failures,
	imported_from::text,imported_through::text,source_revision,resume_period,resume_remaining`

func scanImport(row pgx.Row) (*importRow, error) {
	var r importRow
	err := row.Scan(&r.ProviderID, &r.Source, &r.CredentialID, &r.Scope, &r.Enabled, &r.ETag,
		&r.NextImportAt, &r.LastImportAt, &r.LastSuccessAt, &r.LastErrorCode, &r.ConsecutiveFailures,
		&r.ImportedFrom, &r.ImportedThrough, &r.SourceRevision, &r.ResumePeriod, &r.ResumeRemaining)
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
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
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
		return *replayed, tx.Commit(r.Context())
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
	scope := json.RawMessage(`{}`)
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
		secret, err := readCredentialInput(source, input.Credential)
		if err != nil {
			return access.Reply{}, err
		}
		credential = &secret
	}
	switch {
	case credentialNull:
		return access.Reply{}, access.Invalid("credential", "credential cannot be null; delete the billing import instead")
	case credential != nil:
		newID, err := ImportCredential(r.Context(), tx, s.Access.Keys, s.Access.Installation, source, *credential)
		if err != nil {
			return access.Reply{}, err
		}
		credentialID = &newID
	case credentialID == nil:
		return access.Reply{}, access.Invalid("credential", "credential is required to create a billing import")
	default:
		compatible, err := ImportCredentialCompatible(r.Context(), tx, s.Access.Keys, s.Access.Installation, source, credentialID)
		if err != nil {
			return access.Reply{}, err
		}
		if !compatible {
			return access.Reply{}, access.Invalid("credential", "supply a credential compatible with the billing source")
		}
	}
	newETag, err := SaveImport(r.Context(), tx, id, ImportConfiguration{Source: source, Enabled: enabled, Scope: scope, CredentialID: credentialID}, p.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	action := "provider.billing_import.create"
	status := http.StatusCreated
	if existing != nil {
		action = "provider.billing_import.update"
		status = http.StatusOK
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
	return result, tx.Commit(r.Context())
}

func existingCredential(r *importRow) *string {
	if r == nil {
		return nil
	}
	return r.CredentialID
}

func (s *Server) deleteImport(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
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
	if err := RemoveImport(r.Context(), tx, id); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.billing_import.delete", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: 204}, tx.Commit(r.Context())
}

func (s *Server) runImport(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
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
		return *replayed, tx.Commit(r.Context())
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
	return result, tx.Commit(r.Context())
}

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
