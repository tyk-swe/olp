// Package providers owns provider connections: configuration drafts, model
// discovery and certification, credential pools, activation into immutable
// revisions, and history for the connector matrix.
package providers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
)

// QuotaSource reports the shared quota state a provider connection and its
// credential slots are in right now. *fleet.State implements it. The field
// stays nil where no Valkey is configured, and the console is then told the
// live counters are unknown rather than shown a zero it would read as idle.
type QuotaSource interface {
	Usage(ctx context.Context, lookup string) (fleet.Usage, error)
	Cooling(ctx context.Context, scopes ...string) (bool, error)
	Cooldown(ctx context.Context, scope string, d time.Duration) error
}

type Deps struct {
	Access          *access.Server
	Egress          *egress.Policy
	Signer          connectors.Signer
	ExternalSecrets release.ExternalSecrets
	Unconfined      *plugins.Unconfined
	Quotas          QuotaSource
	Plugins         *plugins.Host
	Catalog         *catalog.Signed
	Activated       func(context.Context, pgx.Tx, string, string) error
	Log             *slog.Logger
}

type Server struct {
	access     *access.Server
	egress     *egress.Policy
	external   release.ExternalSecrets
	unconfined *plugins.Unconfined
	quota      QuotaSource
	plugins    *plugins.Host
	catalog    *catalog.Signed
	activated  func(context.Context, pgx.Tx, string, string) error
	log        *slog.Logger
	prober     *probe.Prober
}

func New(deps Deps) *Server {
	return &Server{
		access: deps.Access, egress: deps.Egress, external: deps.ExternalSecrets, unconfined: deps.Unconfined,
		quota: deps.Quotas, plugins: deps.Plugins, catalog: deps.Catalog, activated: deps.Activated, log: deps.Log,
		prober: probe.New(deps.Egress, deps.Signer, deps.Plugins, networkSecrets{server: deps.Access}),
	}
}

// Register mounts the provider surface.
func (s *Server) Register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/provider-profiles", s.profiles)
	s.access.Route(mux, "GET /api/v1/operation-dialects", s.operationDialects)
	s.access.Route(mux, "GET /api/v1/provider-kinds", s.kinds)
	s.access.Route(mux, "GET /api/v1/provider-kinds/{provider_kind}/capabilities", s.kindCapabilities)
	s.access.Route(mux, "GET /api/v1/provider-vendors", s.listVendors)
	s.access.Route(mux, "GET /api/v1/provider-models", s.inventory)
	s.access.Route(mux, "GET /api/v1/runtime-generations", s.generations)
	s.access.Route(mux, "GET /api/v1/providers", s.providers)
	s.access.Route(mux, "POST /api/v1/providers", s.createProvider, access.MaxBody(1<<20))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}", s.provider)
	s.access.Route(mux, "PATCH /api/v1/providers/{provider_id}", s.updateProvider, access.MaxBody(1<<20))
	s.access.Route(mux, "DELETE /api/v1/providers/{provider_id}", s.deleteProvider)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/activate", s.activateProvider)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/disable", s.disableProvider)
	// Each advertised capability, or each declared model of a provider without
	// discovery, can consume a full probe budget; reserve another management
	// budget for preparation, queueing, and persistence.
	certifyTimeout := time.Duration(len(connection.Vocabulary())+1) * probe.Timeout
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/probe", s.probe, access.Deadline(certifyTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/discovery", s.discover, access.MaxBody(1<<20), access.Deadline(certifyTimeout))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/catalog-suggestions", s.catalogSuggestions)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/catalog-suggestions/accept", s.acceptCatalogSuggestions, access.MaxBody(1<<20))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/restore-as-draft", s.restoreActiveAsDraft)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/models", s.models)
	s.access.Route(mux, "PATCH /api/v1/providers/{provider_id}/models/{model_id}", s.setModel)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/models/{model_id}/certify", s.certify, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/network-credentials", s.networkCredentials)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/network-credentials", s.createNetworkCredential, access.MaxBody(256<<10))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/network-credentials/{credential_id}/revoke", s.revokeNetworkCredential)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/credentials", s.credentials)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/credentials", s.rotate, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/credentials/{credential_id}/revoke", s.revoke)
	for _, adapter := range []codemode.Adapter{codemode.AdapterCodex, codemode.AdapterClaude, codemode.AdapterAntigravity} {
		s.access.Route(mux, "POST /api/v1/providers/{provider_id}/"+string(adapter)+"-import", s.importCredentials(adapter), access.MaxBody(128<<10), access.Deadline(grantStepTimeout))
	}
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments", s.startGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/continue", s.continueGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/poll", s.pollGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.access.Route(mux, "DELETE /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}", s.cancelGrantEnrollment)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/credential-slots", s.slots)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.getSlot)
	s.access.Route(mux, "DELETE /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.deleteSlot)
	s.access.Route(mux, "PUT /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.writeSlot)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/credential-slots/{slot_id}/validate", s.validateSlot, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions", s.revisions)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/diff", s.revisionDiff)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/{revision_id}", s.revision)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/{revision_id}/models", s.revisionModels)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft", s.restoreRevisionAsDraft)
}
