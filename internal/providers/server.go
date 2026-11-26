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
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
)

// Quotas is nil where no Valkey is configured, and the console then shows the
// live counters as unknown instead of an idle zero.
type QuotaSource interface {
	Usage(ctx context.Context, lookup string) (fleet.Usage, error)
	Cooling(ctx context.Context, scopes ...string) (bool, error)
	Cooldown(ctx context.Context, scope string, d time.Duration) error
}

type Deps struct {
	Access          *access.Server
	Egress          *egress.Policy
	Getenv          func(string) string
	ExternalSecrets release.ExternalSecrets
	Unconfined      *plugins.Unconfined
	Quotas          QuotaSource
	Plugins         *plugins.Host
	Catalog         *catalog.Signed
	Activated       func(context.Context, pgx.Tx, string, string) error
	Log             *slog.Logger
}

type Server struct {
	access      *access.Server
	drafts      *draftService
	credentials *credentialService
	slots       *slotService
	models      *modelService
	inventory   *inventoryService
	enrollments *enrollmentService
	revisions   *revisionService
}

type draftService struct {
	access      *access.Server
	egress      *egress.Policy
	unconfined  *plugins.Unconfined
	activated   func(context.Context, pgx.Tx, string, string) error
	credentials *credentialService
}

type credentialService struct {
	access   *access.Server
	external release.ExternalSecrets
	prober   *probe.Prober
}

type slotService struct {
	access      *access.Server
	quota       QuotaSource
	prober      *probe.Prober
	log         *slog.Logger
	credentials *credentialService
}

type modelService struct {
	access      *access.Server
	catalog     *catalog.Signed
	prober      *probe.Prober
	quota       QuotaSource
	log         *slog.Logger
	credentials *credentialService
	drafts      *draftService
}

type inventoryService struct {
	access     *access.Server
	catalog    *catalog.Signed
	unconfined *plugins.Unconfined
}

type enrollmentService struct {
	access  *access.Server
	plugins *plugins.Host
	prober  *probe.Prober
}

type revisionService struct {
	access *access.Server
}

func New(deps Deps) *Server {
	prober := probe.New(deps.Egress, deps.Getenv, deps.Plugins, networkSecrets{server: deps.Access})
	credentials := &credentialService{access: deps.Access, external: deps.ExternalSecrets, prober: prober}
	drafts := &draftService{access: deps.Access, egress: deps.Egress, unconfined: deps.Unconfined, activated: deps.Activated, credentials: credentials}
	return &Server{
		access:      deps.Access,
		drafts:      drafts,
		credentials: credentials,
		slots:       &slotService{access: deps.Access, quota: deps.Quotas, prober: prober, log: deps.Log, credentials: credentials},
		models:      &modelService{access: deps.Access, catalog: deps.Catalog, prober: prober, quota: deps.Quotas, log: deps.Log, credentials: credentials, drafts: drafts},
		inventory:   &inventoryService{access: deps.Access, catalog: deps.Catalog, unconfined: deps.Unconfined},
		enrollments: &enrollmentService{access: deps.Access, plugins: deps.Plugins, prober: prober},
		revisions:   &revisionService{access: deps.Access},
	}
}

func (s *Server) Register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/provider-profiles", s.inventory.profiles)
	s.access.Route(mux, "GET /api/v1/operation-dialects", s.inventory.operationDialects)
	s.access.Route(mux, "GET /api/v1/provider-kinds", s.inventory.kinds)
	s.access.Route(mux, "GET /api/v1/provider-kinds/{provider_kind}/capabilities", s.inventory.kindCapabilities)
	s.access.Route(mux, "GET /api/v1/provider-vendors", s.inventory.listVendors)
	s.access.Route(mux, "GET /api/v1/provider-models", s.inventory.inventory)
	s.access.Route(mux, "GET /api/v1/providers", s.drafts.providers)
	s.access.Route(mux, "POST /api/v1/providers", s.drafts.createProvider, access.MaxBody(1<<20))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}", s.drafts.provider)
	s.access.Route(mux, "PATCH /api/v1/providers/{provider_id}", s.drafts.updateProvider, access.MaxBody(1<<20))
	s.access.Route(mux, "DELETE /api/v1/providers/{provider_id}", s.drafts.deleteProvider)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/activate", s.drafts.activateProvider)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/disable", s.drafts.disableProvider)
	// Each advertised capability, or each declared model of a provider without
	// discovery, can consume a full probe budget; reserve another management
	// budget for preparation, queueing, and persistence.
	certifyTimeout := time.Duration(len(connection.Vocabulary())+1) * probe.Timeout
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/probe", s.models.probe, access.Deadline(certifyTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/discovery", s.models.discover, access.MaxBody(1<<20), access.Deadline(certifyTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/listing-comparison", s.models.compareListing, access.Deadline(certifyTimeout))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/catalog-suggestions", s.models.catalogSuggestions)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/catalog-suggestions/accept", s.models.acceptCatalogSuggestions, access.MaxBody(1<<20))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/restore-as-draft", s.drafts.restoreActiveAsDraft)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/models", s.models.models)
	s.access.Route(mux, "PATCH /api/v1/providers/{provider_id}/models/{model_id}", s.models.setModel)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/models/{model_id}/certify", s.models.certify, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/network-credentials", s.credentials.networkCredentials)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/network-credentials", s.credentials.createNetworkCredential, access.MaxBody(256<<10))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/network-credentials/{credential_id}/revoke", s.credentials.revokeNetworkCredential)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/credentials", s.credentials.credentials)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/credentials", s.credentials.rotate, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/credentials/{credential_id}/revoke", s.credentials.revoke)
	for _, adapter := range []codemode.Adapter{codemode.AdapterCodex, codemode.AdapterClaude, codemode.AdapterAntigravity} {
		s.access.Route(mux, "POST /api/v1/providers/{provider_id}/"+string(adapter)+"-import", s.enrollments.importCredentials(adapter), access.MaxBody(128<<10), access.Deadline(grantStepTimeout))
	}
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments", s.enrollments.startGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/continue", s.enrollments.continueGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/poll", s.enrollments.pollGrantEnrollment, access.MaxBody(65536), access.Deadline(grantStepTimeout))
	s.access.Route(mux, "DELETE /api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}", s.enrollments.cancelGrantEnrollment)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/credential-slots", s.slots.slots)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.slots.getSlot)
	s.access.Route(mux, "DELETE /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.slots.deleteSlot)
	s.access.Route(mux, "PUT /api/v1/providers/{provider_id}/credential-slots/{slot_id}", s.slots.writeSlot)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/credential-slots/{slot_id}/validate", s.slots.validateSlot, access.MaxBody(65536), access.Deadline(certifyTimeout))
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions", s.revisions.revisions)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/diff", s.revisions.revisionDiff)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/{revision_id}", s.revisions.revision)
	s.access.Route(mux, "GET /api/v1/providers/{provider_id}/revisions/{revision_id}/models", s.revisions.revisionModels)
	s.access.Route(mux, "POST /api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft", s.drafts.restoreRevisionAsDraft)
}

func (s *Server) ConnectionPolicy(q database.Queryer) connection.Policy {
	return s.drafts.connectionPolicy(q)
}

func (s *Server) CreateDraft(ctx context.Context, tx pgx.Tx, draft Draft) (CreatedDraft, error) {
	return s.drafts.createDraftRecord(ctx, tx, draft)
}

func (s *Server) StoreDraftConfiguration(ctx context.Context, tx pgx.Tx, id, name string, prepared connection.Prepared) error {
	_, err := s.drafts.storeDraftConfiguration(ctx, tx, id, name, prepared)
	return err
}

func (s *Server) StageDraftInventory(ctx context.Context, tx pgx.Tx, providerID string, models []DraftModel, slots []DraftSlot) error {
	return s.drafts.stageDraftInventory(ctx, tx, providerID, models, slots)
}

func (s *Server) BindDraftCredential(ctx context.Context, tx pgx.Tx, providerID, slotID, credentialID string) error {
	return s.credentials.bindDraftCredential(ctx, tx, providerID, slotID, credentialID)
}

func (s *Server) CredentialEquals(ctx context.Context, q database.Queryer, credentialID, secret string) (bool, error) {
	return s.credentials.credentialEquals(ctx, q, credentialID, secret)
}

func (s *Server) ExternalCredentialEquals(ctx context.Context, q database.Queryer, credentialID string, reference secretstore.Reference) (bool, error) {
	return s.credentials.externalCredentialEquals(ctx, q, credentialID, reference)
}

func (s *Server) StoreCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, int, error) {
	return s.credentials.storePlainCredential(ctx, tx, providerID, secret)
}

func (s *Server) StoreExternalCredential(ctx context.Context, tx pgx.Tx, providerID string, reference secretstore.Reference) (string, int, error) {
	return s.credentials.storeExternalCredential(ctx, tx, providerID, reference)
}

func (s *Server) StoreNetworkCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error) {
	return s.credentials.storeNetworkCredential(ctx, tx, providerID, secret)
}
