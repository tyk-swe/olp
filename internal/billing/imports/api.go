package imports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing/reconciliation"
	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/management/contract"
)

var (
	errProviderMissing = access.Fail(http.StatusNotFound, "not_found", "The provider does not exist.")
	errImportMissing   = access.Fail(http.StatusNotFound, "not_found", "The provider has no billing import.")
)

type API struct {
	server *access.Server
	vault  Vault
}

func NewAPI(server *access.Server) *API {
	return &API{server: server, vault: Vault{Keys: server.Keys, Installation: server.Installation}}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/providers/{provider_id}/billing-import", a.get)
	a.server.Route(mux, "PUT /api/v1/providers/{provider_id}/billing-import", a.put)
	a.server.Route(mux, "DELETE /api/v1/providers/{provider_id}/billing-import", a.remove)
	a.server.Route(mux, "POST /api/v1/providers/{provider_id}/billing-import/run", a.run)
	a.server.Route(mux, "GET /api/v1/providers/{provider_id}/invoiced-costs", a.invoicedCosts)
}

func (a *API) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if err := a.visible(r.Context(), p, id); err != nil {
		return access.Reply{}, err
	}
	current, found, err := readRecord(r.Context(), a.server.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if !found {
		return access.Reply{}, errImportMissing
	}
	body, err := document(current)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(body, current.ETag), nil
}

type putInput struct {
	Enabled    *bool           `json:"enabled"`
	Scope      json.RawMessage `json:"scope"`
	Credential json.RawMessage `json:"credential"`
}

func (a *API) put(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input putInput
	if err := access.DecodeUnique(r, &input, 65536); err != nil {
		return access.Reply{}, err
	}
	if bytes.Equal(bytes.TrimSpace(input.Credential), []byte("null")) {
		return access.Reply{}, access.Invalid("credential", "credential cannot be null; delete the billing import instead")
	}
	return access.Mutate(a.server, r, input, a.changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, owner provider) (access.Reply, error) {
		existing, found, err := lockRecord(r.Context(), tx, id)
		if err != nil {
			return access.Reply{}, err
		}
		if err := access.Match(r, matchETag(owner, existing, found)); err != nil {
			return access.Reply{}, err
		}
		desired, err := parseDesired(owner, existing, input)
		if err != nil {
			return access.Reply{}, err
		}
		etag, err := Save(r.Context(), tx, a.vault, id, desired, p.UserID())
		if err != nil {
			return access.Reply{}, credentialInvalid(err)
		}
		if found {
			return a.reply(r, tx, p, id, "provider.billing_import.update", http.StatusOK, etag)
		}
		return a.reply(r, tx, p, id, "provider.billing_import.create", http.StatusCreated, etag)
	})
}

func matchETag(owner provider, existing record, found bool) string {
	if found {
		return existing.ETag
	}
	return owner.ETag
}

func parseDesired(owner provider, existing record, input putInput) (Desired, error) {
	src, err := source.For(owner.Configuration.Kind)
	if err != nil {
		return Desired{}, err
	}
	scope, err := src.Scope(cmpBytes(input.Scope, existing.Scope, json.RawMessage(`{}`)))
	if err != nil {
		return Desired{}, err
	}
	desired := Desired{Enabled: input.Enabled == nil || *input.Enabled, Scope: scope}
	if len(input.Credential) == 0 {
		return desired, nil
	}
	credential, err := src.Credential(input.Credential)
	if err != nil {
		return Desired{}, err
	}
	return Desired{Enabled: desired.Enabled, Scope: scope, Credential: &credential}, nil
}

func cmpBytes(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func credentialInvalid(err error) error {
	switch {
	case errors.Is(err, ErrCredentialRequired):
		return access.Invalid("credential", "credential is required to create a billing import")
	case errors.Is(err, ErrCredentialIncompatible):
		return access.Invalid("credential", "supply a credential compatible with the billing source")
	}
	return err
}

func (a *API) remove(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if _, err := a.changeable(r.Context(), id)(tx, p); err != nil {
		return access.Reply{}, err
	}
	existing, found, err := lockRecord(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if !found {
		return access.Reply{}, errImportMissing
	}
	if err := access.Match(r, existing.ETag); err != nil {
		return access.Reply{}, err
	}
	if err := Remove(r.Context(), tx, id); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), "provider.billing_import.delete", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusNoContent}, tx.Commit(r.Context())
}

func (a *API) run(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	return access.Mutate(a.server, r, nil, a.changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, _ provider) (access.Reply, error) {
		etag := access.NewID()
		found, err := scheduleNow(r.Context(), tx, id, etag)
		if err != nil {
			return access.Reply{}, err
		}
		if !found {
			return access.Reply{}, errImportMissing
		}
		return a.reply(r, tx, p, id, "provider.billing_import.run", http.StatusAccepted, etag)
	})
}

func (a *API) reply(r *http.Request, tx pgx.Tx, p access.Principal, id, action string, status int, etag string) (access.Reply, error) {
	if err := access.Audit(r.Context(), tx, r, p.Actor(), action, "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	current, _, err := readRecord(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	body, err := document(current)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: status, ETag: etag, Body: body}, nil
}

func (a *API) visible(ctx context.Context, p access.Principal, id string) error {
	owner, found, err := readProvider(ctx, a.server.Pool, id)
	if err != nil {
		return err
	}
	if !found {
		return errProviderMissing
	}
	return p.Project(owner.ProjectID, access.View)
}

func (a *API) changeable(ctx context.Context, id string) func(pgx.Tx, access.Principal) (provider, error) {
	return func(tx pgx.Tx, p access.Principal) (provider, error) {
		owner, found, err := lockProvider(ctx, tx, id)
		if err != nil {
			return provider{}, err
		}
		if !found {
			return provider{}, errProviderMissing
		}
		return owner, p.Project(owner.ProjectID, access.Change)
	}
}

type importStatus = struct {
	ImportedFrom    nullable.Nullable[openapi_types.Date] `json:"imported_from"`
	ImportedThrough nullable.Nullable[openapi_types.Date] `json:"imported_through"`
	LastErrorCode   nullable.Nullable[string]             `json:"last_error_code"`
	LastImportAt    nullable.Nullable[time.Time]          `json:"last_import_at"`
	LastSuccessAt   nullable.Nullable[time.Time]          `json:"last_success_at"`
	NextImportAt    nullable.Nullable[time.Time]          `json:"next_import_at"`
	SourceRevision  nullable.Nullable[string]             `json:"source_revision"`
}

func document(r record) (contract.BillingImport, error) {
	var scope contract.BillingScope
	if err := scope.UnmarshalJSON(r.Scope); err != nil {
		return contract.BillingImport{}, err
	}
	return contract.BillingImport{
		CredentialConfigured: r.CredentialID != nil,
		Enabled:              r.Enabled,
		Etag:                 r.ETag,
		ProviderId:           r.ProviderID,
		Scope:                scope,
		Source:               r.Source,
		Status: importStatus{
			ImportedFrom:    nullableOf(dateOf(r.ImportedFrom)),
			ImportedThrough: nullableOf(dateOf(r.ImportedThrough)),
			LastErrorCode:   nullableOf(r.LastErrorCode),
			LastImportAt:    nullableOf(r.LastImportAt),
			LastSuccessAt:   nullableOf(r.LastSuccessAt),
			NextImportAt:    nullableOf(r.NextImportAt),
			SourceRevision:  nullableOf(r.SourceRevision),
		},
	}, nil
}

func dateOf(value *time.Time) *openapi_types.Date {
	if value == nil {
		return nil
	}
	return &openapi_types.Date{Time: *value}
}

func nullableOf[T any](value *T) nullable.Nullable[T] {
	if value == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*value)
}

type invoicedCost = struct {
	Amount    string             `json:"amount"`
	Currency  string             `json:"currency"`
	Day       openapi_types.Date `json:"day"`
	FetchedAt time.Time          `json:"fetched_at"`
	LineItem  string             `json:"line_item"`
	Source    string             `json:"source"`
}

func (a *API) invoicedCosts(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if err := a.visible(r.Context(), p, id); err != nil {
		return access.Reply{}, err
	}
	window, err := reconciliation.ParseRange(r.URL.Query())
	if err != nil {
		return access.Reply{}, err
	}
	items, err := invoiced(r.Context(), a.server.Pool, id, window)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusOK, Body: contract.InvoicedCostListResponse{Items: items}}, nil
}
