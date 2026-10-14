// Package billing imports vendor invoices and reconciles them against the
// proxy's own accounting. One billing import per provider polls the vendor's
// cost endpoint, stores the invoiced per-day line items, and the
// reconciliation report compares them to the spend accounting attributed to
// that provider.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

// Server is the billing surface of the management API.
type Server struct {
	Access *access.Server
	Egress *egress.Policy
}

// Register mounts the billing routes; their patterns are more specific than
// the management catch-all.
func (s *Server) Register(mux *http.ServeMux) {
	a := s.Access
	a.Route(mux, "GET /api/v1/providers/{provider_id}/billing-import", s.getImport)
	a.Route(mux, "PUT /api/v1/providers/{provider_id}/billing-import", s.putImport)
	a.Route(mux, "DELETE /api/v1/providers/{provider_id}/billing-import", s.deleteImport)
	a.Route(mux, "POST /api/v1/providers/{provider_id}/billing-import/run", s.runImport)
	a.Route(mux, "GET /api/v1/providers/{provider_id}/invoiced-costs", s.invoicedCosts)
	a.Route(mux, "GET /api/v1/usage/reconciliation", s.reconciliation)
}

// provider is the slice of olp.providers billing reads.
type provider struct {
	ID            string
	Name          string
	Kind          string
	ETag          string
	ProjectID     *string
	Endpoint      *string
	Network       json.RawMessage
	CredentialOID *string
}

// providerConfig decodes the stored configuration document for the fields the
// importer needs: the vendor base URL and the connection's network options.
type providerConfig struct {
	Kind     string  `json:"kind"`
	Endpoint *string `json:"endpoint"`
	Options  struct {
		Network *egress.ConnectionOptions `json:"network"`
	} `json:"options"`
}

var errProviderMissing = access.Fail(404, "not_found", "The provider does not exist.")

// loadProvider reads one provider, locking the row inside a transaction when
// asked. A missing provider answers 404.
func loadProvider(ctx context.Context, q access.Queryer, id string, lock bool) (*provider, error) {
	query := `SELECT id::text,name,etag::text,project_id::text,configuration FROM olp.providers WHERE id=$1::uuid`
	if lock {
		query += " FOR UPDATE"
	}
	var p provider
	var configuration []byte
	err := q.QueryRow(ctx, query, id).Scan(&p.ID, &p.Name, &p.ETag, &p.ProjectID, &configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errProviderMissing
	}
	if err != nil {
		return nil, err
	}
	var cfg providerConfig
	if err := json.Unmarshal(configuration, &cfg); err != nil {
		return nil, err
	}
	p.Kind = cfg.Kind
	p.Endpoint = cfg.Endpoint
	if cfg.Options.Network != nil {
		p.Network, _ = json.Marshal(cfg.Options.Network)
	}
	return &p, nil
}

// visibleProvider loads a provider the principal may see; one outside its
// scope answers like one that does not exist.
func visibleProvider(ctx context.Context, q access.Queryer, p access.Principal, id string) (*provider, error) {
	current, err := loadProvider(ctx, q, id, false)
	if err != nil {
		return nil, err
	}
	if err = p.Project(current.ProjectID, access.View); err != nil {
		return nil, err
	}
	return current, nil
}
