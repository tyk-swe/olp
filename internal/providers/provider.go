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
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

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

func (s *draftService) scanDetail(row pgx.Row) (*detail, error) {
	var p record
	var configuration, plugin []byte
	var d detail
	var usableCredential bool
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &configuration, &p.ETag, &p.SlotsETag, &p.DraftDirty, &p.ActiveRevision, &p.ActiveRevisionID, &p.LastProbeAt, &p.LastProbeStatus, &p.LastProbeDetail, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.ProjectID, &plugin,
		&d.ProjectName, &d.CreatedByEmail, &d.ModelCount, &d.EnabledModelCount, &d.CapabilityCount, &d.CertifiedCapabilityCount,
		&d.DraftCredentialID, &d.DraftCredentialVersion, &usableCredential, &d.RuntimeCredentialID, &d.RuntimeCredentialVersion)
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
	d.LastProbeAt, d.LastProbeStatus, d.LastProbeDetail = p.LastProbeAt, p.LastProbeStatus, p.LastProbeDetail
	d.CreatedAt, d.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	d.ConnectorReady = p.Connection.Transport().Validate(s.egress) == nil && (!document.CredentialRequired() || usableCredential)
	return &d, nil
}

func (s *draftService) detail(ctx context.Context, q database.Queryer, id string) (*detail, error) {
	return s.scanDetail(q.QueryRow(ctx, detailQuery+" WHERE p.id=$1", id))
}

func (s *draftService) visibleDetail(ctx context.Context, q database.Queryer, p authz.Principal, id string) (*detail, error) {
	d, err := s.detail(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err := p.Project(d.ProjectID, authz.View); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *draftService) detailReply(ctx context.Context, q database.Queryer, id string) (httpkit.Reply, error) {
	d, err := s.detail(ctx, q, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, d.ETag), nil
}

func (s *draftService) providers(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 100 {
		return httpkit.Reply{}, problem.Invalid("search", "Use at most 100 characters.")
	}
	rows, err := s.access.Pool.Query(r.Context(), detailQuery+" WHERE p.id<$1 AND ($2='' OR p.name ILIKE '%'||$2||'%') AND ($3 OR p.project_id=ANY($4::uuid[])) ORDER BY p.id DESC LIMIT $5", page.Before, search, principal.AllProjects, principal.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	var items []providerSummary
	for rows.Next() {
		d, err := s.scanDetail(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, d.providerSummary)
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item providerSummary) string { return item.ID }), nil
}

func (s *draftService) provider(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := s.visibleDetail(r.Context(), s.access.Pool, p, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, d.ETag), nil
}

func ReadProviderNames(ctx context.Context, q database.Queryer) (map[string]string, error) {
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

func ReadProviderProjects(ctx context.Context, q database.Queryer) (map[string]*string, error) {
	rows, err := q.Query(ctx, "SELECT id::text,project_id::text FROM olp.providers")
	if err != nil {
		return nil, err
	}
	type scoped struct {
		ID      string
		Project *string
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[scoped])
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, *string) bool) {
		for _, entry := range entries {
			if !yield(entry.ID, entry.Project) {
				return
			}
		}
	}), nil
}

// Deletion is deliberately separate from disable: it removes an unused draft,
// while published revisions and retained evidence remain addressable.
func (s *draftService) deleteProvider(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Remove(s.access, r, map[string]string{"provider_id": id}, changeable(r.Context(), id), func(tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var used bool
		if err := tx.QueryRow(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM olp.provider_revisions WHERE provider_id=$1)
 OR EXISTS(SELECT 1 FROM olp.route_drafts d,jsonb_array_elements(d.targets) t WHERE t->>'provider_id'=$1::text)
 OR EXISTS(SELECT 1 FROM olp.prices WHERE provider_id=$1)
 OR EXISTS(SELECT 1 FROM olp.notification_deliveries d JOIN olp.provider_credentials c ON c.id=d.credential_id WHERE c.provider_id=$1)`, id).Scan(&used); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if current.State != stateDraft || used {
			return httpkit.Reply{}, access.Event{}, problem.Fail(409, "provider_in_use", "Disable published providers; deletion requires an unused draft without retained history.")
		}
		if err := grants.DiscardProvider(r.Context(), tx, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		rows, err := tx.Query(r.Context(), `SELECT id::text FROM olp.provider_credentials WHERE provider_id=$1
 UNION SELECT id::text FROM olp.provider_network_credentials WHERE provider_id=$1`, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		secretIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.providers WHERE id=$1", id); err != nil {
			if constraint, ok := errors.AsType[*pgconn.PgError](err); ok && constraint.Code == "23503" {
				return httpkit.Reply{}, access.Event{}, problem.Fail(409, "provider_in_use", "The provider has dependent resources or retained history.")
			}
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = secrets.Delete(r.Context(), tx, secrets.ProviderCredential, secretIDs...); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "provider.delete", Resource: "provider", ID: id, Project: current.ProjectID}, nil
	})
}
