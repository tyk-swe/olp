package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type providerState string

const (
	stateDraft          providerState = "draft"
	stateActive         providerState = "active"
	maxSlots                          = 64
	grantEnrollmentOnly               = "A grant authenticates this provider: its credential versions come from grant enrollment, not a pasted credential."
)

type record struct {
	ID               string
	Name             string
	Kind             string
	State            providerState
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

var recordColumns = "p.id::text,p.name,p.kind,p.state,p.configuration,p.etag::text,p.slots_etag::text,p.draft_dirty,p.active_revision,p.active_revision_id::text,p.last_probe_at,p.last_probe_status,p.last_probe_detail,p.created_by::text,p.created_at,p.updated_at,p.project_id::text," + release.PluginColumn("p")

var recordQuery = "SELECT " + recordColumns + " FROM olp.providers p WHERE p.id=$1"

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

func (p *record) target(credential []byte, probeModels ...string) probe.Target {
	return probe.Target{ProviderID: p.ID, Connection: p.Connection, Credential: credential, ProbeModels: probeModels}
}

func readTx[T any](ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return fn(tx)
}

func readProvider(ctx context.Context, q database.Queryer, id string) (*record, error) {
	return scanRecord(q.QueryRow(ctx, recordQuery, id))
}

func lockProvider(ctx context.Context, tx pgx.Tx, id string) (*record, error) {
	return scanRecord(tx.QueryRow(ctx, recordQuery+" FOR UPDATE", id))
}

// A provider outside p's scope is reported missing.
func visibleProvider(ctx context.Context, q database.Queryer, p authz.Principal, id string) (*record, error) {
	current, err := readProvider(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err = p.Project(current.ProjectID, authz.View); err != nil {
		return nil, err
	}
	return current, nil
}

func touch(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := ids.New()
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag)
	return etag, err
}

func touchSlots(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := ids.New()
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag, ids.New())
	return etag, err
}

func providerMutation(a *access.Server, r *http.Request, action string, input any, fn func(ctx context.Context, tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, error)) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, input, changeable(r.Context(), id), func(tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := fn(r.Context(), tx, p, current)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return reply, access.Event{Action: action, Resource: "provider", ID: id}, nil
	})
}

// A stored replay discloses the provider, so the project check also guards
// replays.
func changeable(ctx context.Context, id string) func(pgx.Tx, authz.Principal) (*record, error) {
	return func(tx pgx.Tx, p authz.Principal) (*record, error) {
		current, err := lockProvider(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return nil, err
		}
		return current, nil
	}
}

// unchangedSince locks a provider that a probe read without the lock and
// refuses to store the outcome once the provider has moved on.
func unchangedSince(ctx context.Context, read *record, during string) func(pgx.Tx, authz.Principal) (*record, error) {
	lock := changeable(ctx, read.ID)
	return func(tx pgx.Tx, p authz.Principal) (*record, error) {
		current, err := lock(tx, p)
		if err != nil {
			return nil, err
		}
		if current.ETag != read.ETag {
			return nil, httpkit.Fail(412, "etag_mismatch", "The connection changed during "+during+"; reload and retry.")
		}
		return current, nil
	}
}

type installedPlugins struct {
	q          database.Queryer
	unconfined *plugins.Unconfined
}

func (p installedPlugins) Profile(ctx context.Context, digest, profileID string) (*connectors.PluginProfile, error) {
	plugin, err := plugins.Profile(ctx, p.q, p.unconfined, digest, profileID)
	if refusal, ok := errors.AsType[*plugins.Error](err); ok {
		return nil, &httpkit.Problem{Status: 422, Code: refusal.Code, Detail: refusal.Message, Field: profileField(refusal.Code)}
	}
	return plugin, err
}

func profileField(code string) string {
	if code == plugins.CodeProfileUnknown {
		return "configuration.profile_id"
	}
	return "configuration.profile_revision"
}

func connectionPolicyFor(policy *egress.Policy, unconfined *plugins.Unconfined, q database.Queryer) connection.Policy {
	return connection.Policy{Egress: policy, Plugins: installedPlugins{q: q, unconfined: unconfined}}
}

func validateNetworkReference(ctx context.Context, q database.Queryer, providerID string, network *egress.ConnectionOptions) error {
	if network == nil || network.CredentialID == "" {
		return nil
	}
	var valid bool
	err := q.QueryRow(ctx, "SELECT revoked_at IS NULL FROM olp.provider_network_credentials WHERE id=$1 AND provider_id=$2", network.CredentialID, providerID).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return httpkit.Invalid("configuration.options.network.credential_id", "Select an unrevoked network credential owned by this provider.")
	}
	return err
}

type storedCapability struct {
	connection.Capability
	Source                string     `json:"source"`
	CertifiedAt           *time.Time `json:"certified_at"`
	CredentialFingerprint string     `json:"credential_fingerprint,omitempty"`
	ToolCalls             bool       `json:"tool_calls,omitempty"`
}

type storedModel struct {
	ID            string             `json:"id"`
	UpstreamModel string             `json:"upstream_model"`
	DisplayName   string             `json:"display_name"`
	Enabled       bool               `json:"enabled"`
	Capabilities  []storedCapability `json:"capabilities"`
	DiscoveredAt  *time.Time         `json:"discovered_at"`
	Listing       *routing.RevisionListing
}

const modelColumns = "id::text,upstream_model,display_name,enabled,capabilities,discovered_at,discovered_listing_endpoint,discovered_listing_api_version,discovered_listing_credential_id::text,discovered_listing_credential_version,discovered_listing_at"

func scanModel(row pgx.Row) (storedModel, error) {
	var m storedModel
	var capabilities []byte
	var listingEndpoint, listingAPIVersion, listingCredentialID *string
	var listingCredentialVersion *int
	var listingAt *time.Time
	if err := row.Scan(&m.ID, &m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities, &m.DiscoveredAt, &listingEndpoint, &listingAPIVersion, &listingCredentialID, &listingCredentialVersion, &listingAt); err != nil {
		return storedModel{}, err
	}
	if listingEndpoint != nil && listingAPIVersion != nil && listingCredentialID != nil && listingCredentialVersion != nil && listingAt != nil {
		m.Listing = &routing.RevisionListing{Endpoint: *listingEndpoint, APIVersion: *listingAPIVersion, CredentialID: *listingCredentialID, CredentialVersion: *listingCredentialVersion, ListedAt: listingAt.UTC()}
	}
	if err := json.Unmarshal(capabilities, &m.Capabilities); err != nil {
		return storedModel{}, err
	}
	if m.Capabilities == nil {
		m.Capabilities = []storedCapability{}
	}
	return m, nil
}

func loadModels(ctx context.Context, q database.Queryer, providerID string) ([]storedModel, error) {
	return queryModels(ctx, q, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 ORDER BY upstream_model", providerID)
}

func enabledModels(ctx context.Context, q database.Queryer, providerID string) ([]storedModel, error) {
	return queryModels(ctx, q, "SELECT "+modelColumns+" FROM olp.provider_models WHERE provider_id=$1 AND enabled ORDER BY upstream_model", providerID)
}

func queryModels(ctx context.Context, q database.Queryer, query string, args ...any) ([]storedModel, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []storedModel
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

type capabilityReply struct {
	connection.Capability
	Source      string     `json:"source"`
	CertifiedAt *time.Time `json:"certified_at"`
	ToolCalls   bool       `json:"tool_calls"`
}

type modelReply struct {
	ID            string            `json:"id"`
	UpstreamModel string            `json:"upstream_model"`
	DisplayName   string            `json:"display_name"`
	Enabled       bool              `json:"enabled"`
	Capabilities  []capabilityReply `json:"capabilities"`
	DiscoveredAt  *time.Time        `json:"discovered_at"`
}

func (m storedModel) reply() modelReply {
	capabilities := make([]capabilityReply, len(m.Capabilities))
	for i, c := range m.Capabilities {
		capabilities[i] = capabilityReply{Capability: c.Capability, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls}
	}
	return modelReply{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: m.Enabled, Capabilities: capabilities, DiscoveredAt: m.DiscoveredAt}
}

func publishedModelReply(m routing.RevisionModel) modelReply {
	capabilities := make([]capabilityReply, len(m.Capabilities))
	for i, c := range m.Capabilities {
		capabilities[i] = capabilityReply{Capability: connection.Capability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode}, Source: c.Source, CertifiedAt: c.CertifiedAt, ToolCalls: c.ToolCalls}
	}
	return modelReply{ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: true, Capabilities: capabilities}
}

func modelID(m modelReply) string { return m.ID }

type credentialSource interface{ credentialSource() }

type pastedCredential string

type externalCredential secretstore.Reference

func (pastedCredential) credentialSource() {}

func (externalCredential) credentialSource() {}

func decodeCredential(plain *string, reference *secretstore.Reference) (credentialSource, error) {
	switch {
	case plain != nil && reference != nil:
		return nil, httpkit.Invalid("credential_reference", "Choose a sealed credential or an external reference.")
	case plain != nil:
		if err := ValidCredential(*plain); err != nil {
			return nil, err
		}
		return pastedCredential(*plain), nil
	case reference != nil:
		if reference.Validate() != nil {
			return nil, httpkit.Invalid("credential_reference", "Use an immutable, version-pinned external credential reference.")
		}
		return externalCredential(*reference), nil
	}
	return nil, nil
}

// External references spend the installation's workload authority. An
// installation operator must approve the exact version for this provider;
// project managers may use approved versions but cannot introduce references.
func authorizeCredential(p authz.Principal, source credentialSource) error {
	if _, external := source.(externalCredential); external {
		return p.Authorize(authz.Settings)
	}
	return nil
}

func referencing(ctx context.Context, id string, source credentialSource) func(pgx.Tx, authz.Principal) (*record, error) {
	lock := changeable(ctx, id)
	return func(tx pgx.Tx, p authz.Principal) (*record, error) {
		if err := authorizeCredential(p, source); err != nil {
			return nil, err
		}
		return lock(tx, p)
	}
}
