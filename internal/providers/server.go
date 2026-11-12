// Package providers owns provider connections: configuration drafts, model
// discovery and certification, credential pools, activation into immutable
// revisions, and history for the connector matrix.
package providers

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/secretstore"
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

type CatalogueOptions struct {
	ExternalSecrets release.ExternalSecrets
	Unconfined      *plugins.Unconfined
	Quotas          QuotaSource
	Plugins         *plugins.Host
	Catalog         *catalog.Signed
	Activated       func(context.Context, pgx.Tx, string, string) error
	Log             *slog.Logger
}

type Catalogue struct {
	Access          *access.Server
	Egress          *egress.Policy
	ExternalSecrets release.ExternalSecrets
	Unconfined      *plugins.Unconfined
	Quotas          QuotaSource
	Plugins         *plugins.Host
	Catalog         *catalog.Signed
	Activated       func(context.Context, pgx.Tx, string, string) error
	Log             *slog.Logger
	prober          *probe.Prober
}

type Server struct {
	*Catalogue
}

func NewCatalogue(a *access.Server, policy *egress.Policy, signer connectors.Signer, getenv func(string) string, options CatalogueOptions) *Catalogue {
	external := options.ExternalSecrets
	if external == nil {
		external = secretstore.New(*policy, getenv)
	}
	return &Catalogue{Access: a, Egress: policy, ExternalSecrets: external, Unconfined: options.Unconfined, Quotas: options.Quotas,
		Plugins: options.Plugins, Catalog: options.Catalog, Activated: options.Activated, Log: options.Log,
		prober: probe.New(policy, signer, options.Plugins, networkSecrets{server: a})}
}

func New(catalogue *Catalogue) *Server {
	return &Server{Catalogue: catalogue}
}

type record struct {
	ID               string
	Name             string
	Kind             string
	State            string
	Connection       connection.Prepared
	ETag             string
	SlotsETag        string
	DraftDirty       bool
	ActiveRevision   *int
	ActiveRevisionID *string
	LastProbeAt      *time.Time
	LastProbeStatus  *string
	LastProbeDetail  *string
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ProjectID        *string
}

const recordColumns = "p.id::text,p.name,p.kind,p.state,p.configuration,p.etag::text,p.slots_etag::text,p.draft_dirty,p.active_revision,p.active_revision_id::text,p.last_probe_at,p.last_probe_status,p.last_probe_detail,p.created_by::text,p.created_at,p.updated_at,p.project_id::text," + pluginColumn

const pluginColumn = "(SELECT jsonb_build_object('manifest', pl.manifest, 'unconfined', pl.executable IS NOT NULL) FROM olp.plugins pl WHERE p.kind='plugin' AND pl.digest=p.configuration->>'profile_revision')"

func scanRecord(row pgx.Row) (*record, error) {
	var p record
	var configuration, plugin []byte
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &configuration, &p.ETag, &p.SlotsETag, &p.DraftDirty, &p.ActiveRevision, &p.ActiveRevisionID, &p.LastProbeAt, &p.LastProbeStatus, &p.LastProbeDetail, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.ProjectID, &plugin)
	if err != nil {
		return nil, err
	}
	p.Connection, err = connection.Load(configuration, plugin)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

const recordQuery = "SELECT " + recordColumns + " FROM olp.providers p WHERE p.id=$1"

func readProvider(ctx context.Context, q access.Queryer, id string) (*record, error) {
	return scanRecord(q.QueryRow(ctx, recordQuery, id))
}

func lockProvider(ctx context.Context, tx pgx.Tx, id string) (*record, error) {
	return scanRecord(tx.QueryRow(ctx, recordQuery+" FOR UPDATE", id))
}

// visibleProvider loads a provider p may see; one outside p's scope is
// missing.
func visibleProvider(ctx context.Context, q access.Queryer, p access.Principal, id string) (*record, error) {
	current, err := readProvider(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err = p.Project(current.ProjectID, access.View); err != nil {
		return nil, err
	}
	return current, nil
}

// touch records a draft change and returns the new etag.
func touch(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag)
	return etag, err
}

// touchSlots records a draft change to the slot set and returns the new etag.
func touchSlots(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag, access.NewID())
	return etag, err
}

// credentialState is what the detail view needs about a credential.
type credentialState struct {
	ID      *string
	Version *int
	Revoked bool
}

type providerSummary struct {
	ID                       string     `json:"id"`
	Name                     string     `json:"name"`
	Kind                     string     `json:"kind"`
	VendorID                 *string    `json:"vendor_id"`
	ProjectID                *string    `json:"project_id"`
	ProjectName              *string    `json:"project_name"`
	State                    string     `json:"state"`
	ConnectorReady           bool       `json:"connector_ready"`
	ETag                     string     `json:"etag"`
	PendingActivation        bool       `json:"pending_activation"`
	ActiveRevision           *int       `json:"active_revision"`
	CreatedByEmail           *string    `json:"created_by_email"`
	LastProbeAt              *time.Time `json:"last_probe_at"`
	LastProbeStatus          *string    `json:"last_probe_status"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	ModelCount               int64      `json:"model_count"`
	EnabledModelCount        int64      `json:"enabled_model_count"`
	CapabilityCount          int64      `json:"capability_count"`
	CertifiedCapabilityCount int64      `json:"certified_capability_count"`
}

type detail struct {
	providerSummary
	Configuration            connection.Configuration `json:"configuration"`
	DraftCredentialID        *string                  `json:"draft_credential_id"`
	DraftCredentialVersion   *int                     `json:"draft_credential_version"`
	RuntimeCredentialID      *string                  `json:"runtime_credential_id"`
	RuntimeCredentialVersion *int                     `json:"runtime_credential_version"`
	LastProbeDetail          *string                  `json:"last_probe_detail"`
}

const detailQuery = "SELECT " + recordColumns + ",pr.name,u.email," +
	"(SELECT count(*) FROM olp.provider_models m WHERE m.provider_id=p.id)," +
	"(SELECT count(*) FROM olp.provider_models m WHERE m.provider_id=p.id AND m.enabled)," +
	"(SELECT coalesce(sum(jsonb_array_length(m.capabilities)),0) FROM olp.provider_models m WHERE m.provider_id=p.id)," +
	"(SELECT count(*) FROM olp.provider_models m,jsonb_array_elements(m.capabilities) c WHERE m.provider_id=p.id AND c->>'source'='certified')," +
	"d.credential_id::text,dc.version,EXISTS(SELECT 1 FROM olp.provider_slots available JOIN olp.provider_credentials secret ON secret.id=available.credential_id WHERE available.provider_id=p.id AND available.enabled AND secret.revoked_at IS NULL),rc.id::text,rc.version" +
	" FROM olp.providers p JOIN olp.users u ON u.id=p.created_by LEFT JOIN olp.projects pr ON pr.id=p.project_id" +
	" LEFT JOIN olp.provider_slots d ON d.provider_id=p.id AND d.is_default" +
	" LEFT JOIN olp.provider_credentials dc ON dc.id=d.credential_id" +
	" LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id" +
	" LEFT JOIN olp.provider_credentials rc ON rc.provider_id=p.id AND rc.version=r.credential_version"

func (s *Catalogue) scanDetail(row pgx.Row) (*detail, error) {
	var p record
	var configuration, plugin []byte
	var d detail
	var draft credentialState
	var usableCredential bool
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &configuration, &p.ETag, &p.SlotsETag, &p.DraftDirty, &p.ActiveRevision, &p.ActiveRevisionID, &p.LastProbeAt, &p.LastProbeStatus, &p.LastProbeDetail, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.ProjectID, &plugin,
		&d.ProjectName, &d.CreatedByEmail, &d.ModelCount, &d.EnabledModelCount, &d.CapabilityCount, &d.CertifiedCapabilityCount,
		&draft.ID, &draft.Version, &usableCredential, &d.RuntimeCredentialID, &d.RuntimeCredentialVersion)
	if err != nil {
		return nil, err
	}
	p.Connection, err = connection.Load(configuration, plugin)
	if err != nil {
		return nil, err
	}
	document := p.Connection.Document()
	d.ID, d.Name, d.Kind, d.State, d.ETag = p.ID, p.Name, p.Kind, p.State, p.ETag
	d.ProjectID = p.ProjectID
	d.VendorID = document.Options.VendorID
	d.Configuration = document
	d.PendingActivation = p.DraftDirty
	d.ActiveRevision = p.ActiveRevision
	d.DraftCredentialID, d.DraftCredentialVersion = draft.ID, draft.Version
	d.LastProbeAt, d.LastProbeStatus, d.LastProbeDetail = p.LastProbeAt, p.LastProbeStatus, p.LastProbeDetail
	d.CreatedAt, d.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	d.ConnectorReady = p.Connection.Transport().Validate(s.Egress) == nil && (!document.CredentialRequired() || usableCredential)
	return &d, nil
}

func (s *Catalogue) detail(ctx context.Context, q access.Queryer, id string) (*detail, error) {
	return s.scanDetail(q.QueryRow(ctx, detailQuery+" WHERE p.id=$1", id))
}

func (s *Server) detailReply(ctx context.Context, q access.Queryer, id string) (access.Reply, error) {
	d, err := s.detail(ctx, q, id)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}

func (p *record) target(credential []byte, probeModels ...string) probe.Target {
	return probe.Target{ProviderID: p.ID, Connection: p.Connection, Credential: credential, ProbeModels: probeModels}
}
