package providers

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/secrets"
)

type providerState string

const (
	stateDraft  providerState = "draft"
	stateActive providerState = "active"
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

var recordQuery = "SELECT " + recordColumns + " FROM olp.providers p WHERE p.id=$1"

func readProvider(ctx context.Context, q access.Queryer, id string) (*record, error) {
	return scanRecord(q.QueryRow(ctx, recordQuery, id))
}

func lockProvider(ctx context.Context, tx pgx.Tx, id string) (*record, error) {
	return scanRecord(tx.QueryRow(ctx, recordQuery+" FOR UPDATE", id))
}

// A provider outside p's scope is reported missing.
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

func touch(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag)
	return etag, err
}

func touchSlots(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=true,updated_at=now() WHERE id=$1", id, etag, access.NewID())
	return etag, err
}

type credentialState struct {
	ID      *string
	Version *int
	Revoked bool
}

type providerSummary struct {
	ID                       string        `json:"id"`
	Name                     string        `json:"name"`
	Kind                     string        `json:"kind"`
	VendorID                 *string       `json:"vendor_id"`
	ProjectID                *string       `json:"project_id"`
	ProjectName              *string       `json:"project_name"`
	State                    providerState `json:"state"`
	ConnectorReady           bool          `json:"connector_ready"`
	ETag                     string        `json:"etag"`
	PendingActivation        bool          `json:"pending_activation"`
	ActiveRevision           *int          `json:"active_revision"`
	CreatedByEmail           *string       `json:"created_by_email"`
	LastProbeAt              *time.Time    `json:"last_probe_at"`
	LastProbeStatus          *string       `json:"last_probe_status"`
	CreatedAt                time.Time     `json:"created_at"`
	UpdatedAt                time.Time     `json:"updated_at"`
	ModelCount               int64         `json:"model_count"`
	EnabledModelCount        int64         `json:"enabled_model_count"`
	CapabilityCount          int64         `json:"capability_count"`
	CertifiedCapabilityCount int64         `json:"certified_capability_count"`
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

var detailQuery = "SELECT " + recordColumns + ",pr.name,u.email," +
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

func (s *Server) scanDetail(row pgx.Row) (*detail, error) {
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
	d.ConnectorReady = p.Connection.Transport().Validate(s.egress) == nil && (!document.CredentialRequired() || usableCredential)
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

func (p *record) target(credential []byte, probeModels ...string) probe.Target {
	return probe.Target{ProviderID: p.ID, Connection: p.Connection, Credential: credential, ProbeModels: probeModels}
}

func (s *Server) providers(r *http.Request, principal access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 100 {
		return access.Reply{}, access.Invalid("search", "Use at most 100 characters.")
	}
	rows, err := s.access.Pool.Query(r.Context(), detailQuery+" WHERE p.id<$1 AND ($2='' OR p.name ILIKE '%'||$2||'%') AND ($3 OR p.project_id=ANY($4::uuid[])) ORDER BY p.id DESC LIMIT $5", page.Before, search, principal.AllProjects, principal.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	var items []providerSummary
	for rows.Next() {
		d, err := s.scanDetail(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, d.providerSummary)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item providerSummary) string { return item.ID }), nil
}

func (s *Server) provider(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	return s.detailReply(r.Context(), s.access.Pool, id)
}

func (s *Server) mutation(r *http.Request, action string, input any, fn func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error)) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	return access.Mutate(s.access, r, input, changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if err := access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		reply, err := fn(r.Context(), tx, p, current)
		if err != nil {
			return access.Reply{}, err
		}
		return reply, access.Audit(r.Context(), tx, r, p.Actor(), action, "provider", id, "success")
	})
}

// A stored replay discloses the provider, so the project check also guards
// replays.
func changeable(ctx context.Context, id string) func(pgx.Tx, access.Principal) (*record, error) {
	return func(tx pgx.Tx, p access.Principal) (*record, error) {
		current, err := lockProvider(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := p.Project(current.ProjectID, access.Change); err != nil {
			return nil, err
		}
		return current, nil
	}
}

// unchangedSince locks a provider that a probe read without the lock and
// refuses to store the outcome once the provider has moved on.
func unchangedSince(ctx context.Context, read *record, during string) func(pgx.Tx, access.Principal) (*record, error) {
	lock := changeable(ctx, read.ID)
	return func(tx pgx.Tx, p access.Principal) (*record, error) {
		current, err := lock(tx, p)
		if err != nil {
			return nil, err
		}
		if current.ETag != read.ETag {
			return nil, access.Fail(412, "etag_mismatch", "The connection changed during "+during+"; reload and retry.")
		}
		return current, nil
	}
}

func valueOr[T any](v *T, fallback T) T {
	if v == nil {
		return fallback
	}
	return *v
}

func deref[T comparable](v *T) T {
	var zero T
	if v == nil {
		return zero
	}
	return *v
}

func ReadProviderNames(ctx context.Context, q access.Queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.providers")
	if err != nil {
		return nil, err
	}
	type named struct{ ID, Name string }
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[named])
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, string) bool) {
		for _, entry := range entries {
			if !yield(entry.ID, entry.Name) {
				return
			}
		}
	}), nil
}

// Deletion is deliberately separate from disable: it removes an unused draft,
// while published revisions and retained evidence remain addressable.
func (s *Server) deleteProvider(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	return access.Remove(s.access, r, map[string]string{"provider_id": id}, changeable(r.Context(), id), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		if err := access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		var used bool
		if err := tx.QueryRow(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM olp.provider_revisions WHERE provider_id=$1)
 OR EXISTS(SELECT 1 FROM olp.route_drafts d,jsonb_array_elements(d.targets) t WHERE t->>'provider_id'=$1::text)
 OR EXISTS(SELECT 1 FROM olp.prices WHERE provider_id=$1)
 OR EXISTS(SELECT 1 FROM olp.notification_deliveries d JOIN olp.provider_credentials c ON c.id=d.credential_id WHERE c.provider_id=$1)`, id).Scan(&used); err != nil {
			return access.Reply{}, err
		}
		if current.State != stateDraft || used {
			return access.Reply{}, access.Fail(409, "provider_in_use", "Disable published providers; deletion requires an unused draft without retained history.")
		}
		if err := grants.DiscardProvider(r.Context(), tx, id); err != nil {
			return access.Reply{}, err
		}
		rows, err := tx.Query(r.Context(), `SELECT id::text FROM olp.provider_credentials WHERE provider_id=$1
 UNION SELECT id::text FROM olp.provider_network_credentials WHERE provider_id=$1`, id)
		if err != nil {
			return access.Reply{}, err
		}
		secretIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return access.Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.providers WHERE id=$1", id); err != nil {
			var constraint *pgconn.PgError
			if errors.As(err, &constraint) && constraint.Code == "23503" {
				return access.Reply{}, access.Fail(409, "provider_in_use", "The provider has dependent resources or retained history.")
			}
			return access.Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=ANY($1::uuid[]) AND purpose=$2", secretIDs, secrets.ProviderCredential); err != nil {
			return access.Reply{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return access.Reply{}, err
		}
		if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), "provider.delete", "provider", id, "success", current.ProjectID); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent}, nil
	})
}
