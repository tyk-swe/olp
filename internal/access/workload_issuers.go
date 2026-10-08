package access

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/http"

	"github.com/tyk-swe/olp/internal/workload"
)

// IdentityHTTPClient applies the independent, bounded identity egress policy.
// Provider network exceptions never broaden this client.
func IdentityHTTPClient() *http.Client { return oidcHTTPClient() }

const workloadIssuerJSON = `i.document||jsonb_build_object('id',i.id,'etag',i.etag,'created_at',i.created_at,'updated_at',i.updated_at,'created_by',i.created_by)`

func (s *Server) workloadIssuers(r *http.Request, _ Principal) (Reply, error) {
	page, e := Page(r)
	if e != nil {
		return Reply{}, e
	}
	rows, e := s.Pool.Query(r.Context(), "SELECT "+workloadIssuerJSON+" FROM olp.workload_issuers i WHERE id<$1 ORDER BY id DESC LIMIT $2", page.Before, page.Limit+1)
	if e != nil {
		return Reply{}, e
	}
	defer rows.Close()
	items, e := JSONRows(rows)
	return ListReply(items, page), e
}
func (s *Server) workloadIssuer(r *http.Request, _ Principal) (Reply, error) {
	id, e := IDParam(r, "issuer_id")
	if e != nil {
		return Reply{}, e
	}
	var data json.RawMessage
	var etag string
	e = s.Pool.QueryRow(r.Context(), "SELECT "+workloadIssuerJSON+",i.etag::text FROM olp.workload_issuers i WHERE id=$1", id).Scan(&data, &etag)
	return Detail(data, etag), e
}
func (s *Server) createWorkloadIssuer(r *http.Request, p Principal) (Reply, error) {
	return s.writeWorkloadIssuer(r, p, true)
}
func (s *Server) putWorkloadIssuer(r *http.Request, p Principal) (Reply, error) {
	return s.writeWorkloadIssuer(r, p, false)
}
func (s *Server) writeWorkloadIssuer(r *http.Request, _ Principal, create bool) (Reply, error) {
	var input workload.Config
	if e := DecodeUnique(r, &input, 65536); e != nil {
		return Reply{}, e
	}
	if err := ValidateWorkloadConfig(input); err != nil {
		return Reply{}, err
	}

	id := NewID()
	if !create {
		var err error
		id, err = IDParam(r, "issuer_id")
		if err != nil {
			return Reply{}, err
		}
	}
	// A key-ID revocation must work even during a JWKS outage. Fetch only for
	// first enablement or a changed endpoint, outside the mutation lock.
	verifyKeys := input.Enabled
	if create && input.Enabled {
		// A completed creation replay must survive a later JWKS outage. Existing
		// issuer URLs cannot be created again, so skipping preflight cannot change trust.
		var exists bool
		if err := s.Pool.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.workload_issuers WHERE issuer=$1)", input.Issuer).Scan(&exists); err != nil {
			return Reply{}, err
		}
		verifyKeys = !exists
	}
	if !create && input.Enabled {
		var oldURL string
		var oldEnabled bool
		if err := s.Pool.QueryRow(r.Context(), "SELECT document->>'jwks_url',(document->>'enabled')::boolean FROM olp.workload_issuers WHERE id=$1", id).Scan(&oldURL, &oldEnabled); err != nil {
			return Reply{}, err
		}
		verifyKeys = !oldEnabled || oldURL != input.JWKSURL
	}
	if verifyKeys {
		if _, err := workload.Fetch(r.Context(), s.OIDCClient, input.JWKSURL); err != nil {
			return Reply{}, Fail(422, "workload_keys_unavailable", "The public signing keys are unavailable or outside identity egress policy.")
		}
	}

	tx, e := s.Begin(r)
	if e != nil {
		return Reply{}, e
	}
	defer tx.Rollback(r.Context())
	p, e := s.Reauthorize(r, tx)
	if e != nil {
		return Reply{}, e
	}
	var replay ReplayClaim
	var oldETag, issuer string
	if create {
		var old *Reply
		replay, old, e = s.Replay(r, tx, p, input)
		if e != nil {
			return Reply{}, e
		}
		if old != nil {
			return Commit(r, tx, *old)
		}
	} else {
		if e = tx.QueryRow(r.Context(), "SELECT etag::text,issuer FROM olp.workload_issuers WHERE id=$1 FOR UPDATE", id).Scan(&oldETag, &issuer); e != nil {
			return Reply{}, e
		}
		if e = Match(r, oldETag); e != nil {
			return Reply{}, e
		}
		if issuer != input.Issuer {
			return Reply{}, Invalid("issuer", "An issuer URL is immutable; disable it and register a new issuer.")
		}
	}
	etag, e := s.StoreWorkloadIssuer(r.Context(), tx, p, id, input, create)
	if e != nil {
		return Reply{}, e
	}
	if e = Audit(r.Context(), tx, r, p.Actor(), "workload_issuer.update", "workload_issuer", id, "success"); e != nil {
		return Reply{}, e
	}
	var output json.RawMessage
	if e = tx.QueryRow(r.Context(), "SELECT "+workloadIssuerJSON+" FROM olp.workload_issuers i WHERE id=$1", id).Scan(&output); e != nil {
		return Reply{}, e
	}
	reply := Detail(output, etag)
	if create {
		reply.Status = 201
		reply.Location = "/api/v1/workload-issuers/" + id
		if e = s.CompleteReplay(r, tx, replay, reply); e != nil {
			return Reply{}, e
		}
	}
	return Commit(r, tx, reply)
}

func WorkloadKeyPolicy(m workload.Mapping) KeyPolicy {
	template := m.LimitTemplate
	return KeyPolicy{Scopes: m.Scopes, AllowedRoutes: []string{}, AllowedAttributionKeys: []string{}, AllowedRouteGroups: m.RouteGroups, LimitTemplate: &template, AllowProviderState: m.AllowProviderState}
}

// ValidateWorkloadConfig checks declared identity URLs without fetching them.
func ValidateWorkloadConfig(input workload.Config) error {
	if input.Validate() != nil {
		return Invalid("issuer", "Supply a bounded issuer, audiences, signature algorithms and explicit claim mappings.")
	}
	for _, raw := range []string{input.Issuer, input.JWKSURL} {
		if oidcURL(raw) != nil {
			return Invalid("issuer", "Use secure identity endpoint URLs.")
		}
	}

	return nil
}

// StoreWorkloadIssuer writes validated identity state in an authorized mutation
// transaction. Callers validate new/enabled endpoints before acquiring the lock.
func (s *Server) StoreWorkloadIssuer(ctx context.Context, tx pgx.Tx, p Principal, id string, input workload.Config, create bool) (string, error) {
	if err := p.Authorize(Access); err != nil {
		return "", err
	}
	if err := ValidateWorkloadConfig(input); err != nil {
		return "", err
	}
	var e error
	for _, mapping := range input.Mappings {
		project := mapping.ProjectID
		if e = s.RequireProject(ctx, tx, p, &project); e != nil {
			return "", e
		}
		if !input.Enabled {
			continue
		}
		if e = checkLimitTemplates(ctx, tx, &project, &mapping.LimitTemplate); e != nil {
			return "", e
		}
		if e = checkRouteGroupNames(ctx, tx, mapping.RouteGroups, &project); e != nil {
			return "", e
		}
	}
	// Existing subjects keep one project and mapping. Updating that mapping's
	// permissions changes all of them through the same authority generation.
	if !create {
		var issuer string
		if err := tx.QueryRow(ctx, "SELECT issuer FROM olp.workload_issuers WHERE id=$1 FOR UPDATE", id).Scan(&issuer); err != nil {
			return "", err
		}
		if issuer != input.Issuer {
			return "", Invalid("issuer", "The issuer URL is immutable.")
		}
		if err := ValidateWorkloadBindings(ctx, tx, id, input); err != nil {
			return "", err
		}
		for _, mapping := range input.Mappings {
			policy, _ := json.Marshal(WorkloadKeyPolicy(mapping))
			if _, err := tx.Exec(ctx, "UPDATE olp.api_keys SET policy=$3,etag=$4 WHERE workload_issuer_id=$1 AND workload_mapping=$2", id, mapping.Name, policy, NewID()); err != nil {
				return "", err
			}
		}
	}
	etag := NewID()
	data, _ := json.Marshal(input)
	if create {
		var count int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM olp.workload_issuers").Scan(&count); e != nil {
			return "", e
		}
		if count >= 64 {
			return "", Invalid("issuer", "At most 64 workload issuers can be registered.")
		}
		_, e = tx.Exec(ctx, "INSERT INTO olp.workload_issuers(id,issuer,document,created_by,etag) VALUES($1,$2,$3,$4,$5)", id, input.Issuer, data, p.UserID(), etag)
	} else {
		_, e = tx.Exec(ctx, "UPDATE olp.workload_issuers SET document=$2,etag=$3,updated_at=now() WHERE id=$1", id, data, etag)
	}
	if e != nil {
		return "", e
	}
	if _, e = AdvanceAuthority(ctx, tx); e != nil {
		return "", e
	}
	return etag, nil
}

// ValidateWorkloadBindings preserves enrolled subjects during planning and writes.
func ValidateWorkloadBindings(ctx context.Context, q Queryer, id string, input workload.Config) error {
	rows, err := q.Query(ctx, "SELECT DISTINCT workload_mapping,project_id::text FROM olp.api_keys WHERE workload_issuer_id=$1", id)
	if err != nil {
		return err
	}
	bindings := map[string]string{}
	for rows.Next() {
		var name, project string
		if err = rows.Scan(&name, &project); err != nil {
			rows.Close()
			return err
		}
		bindings[name] = project
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for name, project := range bindings {
		found := false
		for _, mapping := range input.Mappings {
			if mapping.Name == name && mapping.ProjectID == project {
				found = true
				break
			}
		}
		if !found {
			return Fail(409, "workload_mapping_in_use", "A registered principal's mapping and project cannot be removed or reassigned; disable the issuer to stop access.")
		}
	}
	return nil
}
