package providers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
)

// HealthStats summarises gateway attempts against one provider.
type HealthStats struct {
	Attempts        int64
	Successes       int64
	RateLimits      int64
	ServerErrors    int64
	TransportErrors int64
	TotalLatency    time.Duration
	LastAttemptAt   time.Time
}

// HealthSource reports attempt statistics; nil when this process serves no
// inference.
type HealthSource interface {
	ProviderHealth(window time.Duration) map[string]HealthStats
}

// QuotaSource reports the shared quota state a provider connection and its
// credential slots are in right now. *limits.Limiter implements it. The field
// stays nil where no Valkey is configured, and the console is then told the
// live counters are unknown rather than shown a zero it would read as idle.
type QuotaSource interface {
	ProviderUsage(ctx context.Context, lookup string) (limits.Usage, error)
	Cooling(ctx context.Context, scopes ...string) (bool, error)
}

// Server serves the provider management surface.
type Server struct {
	Access      *access.Server
	Egress      *egress.Policy
	Quotas      QuotaSource
	Log         *slog.Logger
	client      *http.Client
	connections *egress.ConnectionClientCache
	auth        *connectors.Auth
	probes      chan struct{}
}

// New prepares the provider surface with a bounded upstream client and at
// most four concurrent probes.
func New(a *access.Server, policy *egress.Policy) *Server {
	return &Server{connections: egress.NewConnectionClientCache(128), Access: a, Egress: policy, client: policy.Client(probeTimeout), auth: connectors.NewAuth(policy), probes: make(chan struct{}, 4)}
}

type record struct {
	ID               string
	Name             string
	Kind             string
	State            string
	Configuration    Configuration
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

const recordColumns = "p.id::text,p.name,p.kind,p.state,p.configuration,p.etag::text,p.slots_etag::text,p.draft_dirty,p.active_revision,p.active_revision_id::text,p.last_probe_at,p.last_probe_status,p.last_probe_detail,p.created_by::text,p.created_at,p.updated_at,p.project_id::text"

func scanRecord(row pgx.Row) (*record, error) {
	var p record
	var configuration []byte
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &configuration, &p.ETag, &p.SlotsETag, &p.DraftDirty, &p.ActiveRevision, &p.ActiveRevisionID, &p.LastProbeAt, &p.LastProbeStatus, &p.LastProbeDetail, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.ProjectID)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(configuration, &p.Configuration); err != nil {
		return nil, err
	}
	p.Configuration.Normalize()
	p.Configuration.ProviderID = p.ID
	return &p, nil
}

// load reads one provider, locking the row inside a transaction when asked.
func load(ctx context.Context, q access.Queryer, id string, lock bool) (*record, error) {
	query := "SELECT " + recordColumns + " FROM olp_go.providers p WHERE p.id=$1"
	if lock {
		query += " FOR UPDATE"
	}
	return scanRecord(q.QueryRow(ctx, query, id))
}

func checkProvider(ctx context.Context, q access.Queryer, p access.Principal, id string, write bool) (*record, error) {
	current, err := load(ctx, q, id, false)
	if err != nil {
		return nil, err
	}
	if !p.CanProject(current.ProjectID, write) {
		return nil, pgx.ErrNoRows
	}
	return current, nil
}

// touch records a draft change and returns the new etag.
func touch(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp_go.providers SET etag=$2,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag)
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
	Configuration            Configuration `json:"configuration"`
	DraftCredentialID        *string       `json:"draft_credential_id"`
	DraftCredentialVersion   *int          `json:"draft_credential_version"`
	RuntimeCredentialID      *string       `json:"runtime_credential_id"`
	RuntimeCredentialVersion *int          `json:"runtime_credential_version"`
	LastProbeDetail          *string       `json:"last_probe_detail"`
}

const detailQuery = "SELECT " + recordColumns + ",pr.name,u.email," +
	"(SELECT count(*) FROM olp_go.provider_models m WHERE m.provider_id=p.id)," +
	"(SELECT count(*) FROM olp_go.provider_models m WHERE m.provider_id=p.id AND m.enabled)," +
	"(SELECT coalesce(sum(jsonb_array_length(m.capabilities)),0) FROM olp_go.provider_models m WHERE m.provider_id=p.id)," +
	"(SELECT count(*) FROM olp_go.provider_models m,jsonb_array_elements(m.capabilities) c WHERE m.provider_id=p.id AND c->>'source'='certified')," +
	"d.credential_id::text,dc.version,EXISTS(SELECT 1 FROM olp_go.provider_slots available JOIN olp_go.provider_credentials secret ON secret.id=available.credential_id WHERE available.provider_id=p.id AND available.enabled AND secret.revoked_at IS NULL),rc.id::text,rc.version" +
	" FROM olp_go.providers p JOIN olp_go.users u ON u.id=p.created_by LEFT JOIN olp_go.projects pr ON pr.id=p.project_id" +
	" LEFT JOIN olp_go.provider_slots d ON d.provider_id=p.id AND d.is_default" +
	" LEFT JOIN olp_go.provider_credentials dc ON dc.id=d.credential_id" +
	" LEFT JOIN olp_go.provider_revisions r ON r.id=p.active_revision_id" +
	" LEFT JOIN olp_go.provider_credentials rc ON rc.provider_id=p.id AND rc.version=r.credential_version"

func (s *Server) scanDetail(row pgx.Row) (*detail, error) {
	var p record
	var configuration []byte
	var d detail
	var draft credentialState
	var usableCredential bool
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &configuration, &p.ETag, &p.SlotsETag, &p.DraftDirty, &p.ActiveRevision, &p.ActiveRevisionID, &p.LastProbeAt, &p.LastProbeStatus, &p.LastProbeDetail, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.ProjectID,
		&d.ProjectName, &d.CreatedByEmail, &d.ModelCount, &d.EnabledModelCount, &d.CapabilityCount, &d.CertifiedCapabilityCount,
		&draft.ID, &draft.Version, &usableCredential, &d.RuntimeCredentialID, &d.RuntimeCredentialVersion)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(configuration, &p.Configuration); err != nil {
		return nil, err
	}
	p.Configuration.Normalize()
	p.Configuration.ProviderID = p.ID
	d.ID, d.Name, d.Kind, d.State, d.ETag = p.ID, p.Name, p.Kind, p.State, p.ETag
	d.ProjectID = p.ProjectID
	d.VendorID = p.Configuration.Options.VendorID
	d.Configuration = p.Configuration
	d.PendingActivation = p.DraftDirty
	d.ActiveRevision = p.ActiveRevision
	d.DraftCredentialID, d.DraftCredentialVersion = draft.ID, draft.Version
	d.LastProbeAt, d.LastProbeStatus, d.LastProbeDetail = p.LastProbeAt, p.LastProbeStatus, p.LastProbeDetail
	d.CreatedAt, d.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	d.ConnectorReady = p.Configuration.Validate(s.Egress) == nil && (!p.Configuration.CredentialRequired() || usableCredential)
	return &d, nil
}

func (s *Server) detail(ctx context.Context, q access.Queryer, id string) (*detail, error) {
	return s.scanDetail(q.QueryRow(ctx, detailQuery+" WHERE p.id=$1", id))
}

func (s *Server) detailReply(ctx context.Context, q access.Queryer, id string) (access.Reply, error) {
	d, err := s.detail(ctx, q, id)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}
