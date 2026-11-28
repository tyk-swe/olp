package apikeys

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/workload"
)

const workloadIssuerJSON = `i.document||jsonb_build_object('id',i.id,'etag',i.etag,'created_at',i.created_at,'updated_at',i.updated_at,'created_by',i.created_by)`

func (a *API) workloadIssuers(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	page, e := httpkit.Page(r)
	if e != nil {
		return httpkit.Reply{}, e
	}
	items, e := httpkit.QueryJSON(r.Context(), a.server.Pool, "SELECT "+workloadIssuerJSON+" FROM olp.workload_issuers i WHERE id<$1 ORDER BY id DESC LIMIT $2", page.Before, page.Limit+1)
	return httpkit.ListReply(items, page), e
}
func (a *API) workloadIssuer(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, e := httpkit.IDParam(r, "issuer_id")
	if e != nil {
		return httpkit.Reply{}, e
	}
	var data json.RawMessage
	var etag string
	e = a.server.Pool.QueryRow(r.Context(), "SELECT "+workloadIssuerJSON+",i.etag::text FROM olp.workload_issuers i WHERE id=$1", id).Scan(&data, &etag)
	return httpkit.Detail(data, etag), e
}
func decodeWorkloadIssuer(r *http.Request) (workload.Config, error) {
	var input workload.Config
	if err := httpkit.DecodeUnique(r, &input, 65536); err != nil {
		return workload.Config{}, err
	}
	return input, ValidateWorkloadConfig(input)
}

// fetchWorkloadKeys runs outside the mutation lock, and only for first
// enablement or a changed endpoint, so a key-ID revocation still works during
// a JWKS outage.
func (a *API) fetchWorkloadKeys(r *http.Request, input workload.Config) error {
	if _, err := workload.Fetch(r.Context(), a.server.IdentityClient(), input.JWKSURL); err != nil {
		return httpkit.Fail(422, "workload_keys_unavailable", "The public signing keys are unavailable or outside identity egress policy.")
	}
	return nil
}

func (a *API) createWorkloadIssuer(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input, err := decodeWorkloadIssuer(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if input.Enabled {
		// A completed creation replay must survive a later JWKS outage. Existing
		// issuer URLs cannot be created again, so skipping the fetch cannot change trust.
		var exists bool
		if err := a.server.Pool.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.workload_issuers WHERE issuer=$1)", input.Issuer).Scan(&exists); err != nil {
			return httpkit.Reply{}, err
		}
		if !exists {
			if err := a.fetchWorkloadKeys(r, input); err != nil {
				return httpkit.Reply{}, err
			}
		}
	}
	id := ids.New()
	return access.Mutate(a.server, r, input, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		reply, event, err := a.writeWorkloadIssuer(r, tx, p, id, input, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 201, Body: reply.Body, ETag: reply.ETag, Location: "/api/v1/workload-issuers/" + id}, event, nil
	})
}

func (a *API) putWorkloadIssuer(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input, err := decodeWorkloadIssuer(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	id, err := httpkit.IDParam(r, "issuer_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if input.Enabled {
		var keysURL string
		var enabled bool
		if err := a.server.Pool.QueryRow(r.Context(), "SELECT document->>'jwks_url',(document->>'enabled')::boolean FROM olp.workload_issuers WHERE id=$1", id).Scan(&keysURL, &enabled); err != nil {
			return httpkit.Reply{}, err
		}
		if !enabled || keysURL != input.JWKSURL {
			if err := a.fetchWorkloadKeys(r, input); err != nil {
				return httpkit.Reply{}, err
			}
		}
	}
	return access.Transact(a.server, r, func(tx pgx.Tx, _ authz.Principal) (struct{}, error) {
		var etag, issuer string
		if err := tx.QueryRow(r.Context(), "SELECT etag::text,issuer FROM olp.workload_issuers WHERE id=$1 FOR UPDATE", id).Scan(&etag, &issuer); err != nil {
			return struct{}{}, err
		}
		if err := httpkit.Match(r, etag); err != nil {
			return struct{}{}, err
		}
		if issuer != input.Issuer {
			return struct{}{}, httpkit.Invalid("issuer", "An issuer URL is immutable; disable it and register a new issuer.")
		}
		return struct{}{}, nil
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return a.writeWorkloadIssuer(r, tx, p, id, input, false)
	})
}

func (a *API) writeWorkloadIssuer(r *http.Request, tx pgx.Tx, p authz.Principal, id string, input workload.Config, create bool) (httpkit.Reply, access.Event, error) {
	etag, err := StoreWorkloadIssuer(r.Context(), a.server, tx, p, id, input, create)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	var output json.RawMessage
	if err := tx.QueryRow(r.Context(), "SELECT "+workloadIssuerJSON+" FROM olp.workload_issuers i WHERE id=$1", id).Scan(&output); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Detail(output, etag), access.Event{Action: "workload_issuer.update", Resource: "workload_issuer", ID: id}, nil
}

// ValidateWorkloadConfig checks declared identity URLs without fetching them.
func ValidateWorkloadConfig(input workload.Config) error {
	if input.Validate() != nil {
		return httpkit.Invalid("issuer", "Supply a bounded issuer, audiences, signature algorithms and explicit claim mappings.")
	}
	for _, raw := range []string{input.Issuer, input.JWKSURL} {
		if access.CheckIdentityURL(raw) != nil {
			return httpkit.Invalid("issuer", "Use secure identity endpoint URLs.")
		}
	}

	return nil
}

// StoreWorkloadIssuer writes validated identity state in an authorized mutation
// transaction. Callers validate new/enabled endpoints before acquiring the lock.
func StoreWorkloadIssuer(ctx context.Context, server *access.Server, tx pgx.Tx, p authz.Principal, id string, input workload.Config, create bool) (string, error) {
	if err := p.Authorize(authz.Access); err != nil {
		return "", err
	}
	if err := ValidateWorkloadConfig(input); err != nil {
		return "", err
	}
	var previous workload.Config
	if !create {
		var issuer string
		if err := tx.QueryRow(ctx, "SELECT issuer,document FROM olp.workload_issuers WHERE id=$1 FOR UPDATE", id).Scan(&issuer, &previous); err != nil {
			return "", err
		}
		if issuer != input.Issuer {
			return "", httpkit.Invalid("issuer", "The issuer URL is immutable.")
		}
	}
	var e error
	for _, mapping := range input.Mappings {
		project := mapping.ProjectID
		if e = server.RequireProject(ctx, tx, p, &project); e != nil {
			return "", e
		}
		if !input.Enabled {
			continue
		}
		if e = access.CheckLimitTemplates(ctx, tx, &project, &mapping.LimitTemplate); e != nil {
			return "", e
		}
		resources := keyauthority.WorkloadPolicy(mapping)
		if previous.Enabled {
			for _, old := range previous.Mappings {
				if old.Name == mapping.Name && old.ProjectID == mapping.ProjectID {
					resources = addedKeyAgentResources(resources, keyauthority.WorkloadPolicy(old))
					break
				}
			}
		}
		if e = checkKeyAgentResources(ctx, tx, resources, &project); e != nil {
			return "", e
		}
		if e = checkRouteGroupNames(ctx, tx, mapping.RouteGroups, &project); e != nil {
			return "", e
		}
	}
	// Existing subjects keep one project and mapping. Updating that mapping's
	// permissions changes all of them through the same authority generation.
	if !create {
		if err := ValidateWorkloadBindings(ctx, tx, id, input); err != nil {
			return "", err
		}
		for _, mapping := range input.Mappings {
			policy, _ := json.Marshal(keyauthority.WorkloadPolicy(mapping))
			if _, err := tx.Exec(ctx, "UPDATE olp.api_keys SET policy=$3,etag=$4 WHERE workload_issuer_id=$1 AND workload_mapping=$2", id, mapping.Name, policy, ids.New()); err != nil {
				return "", err
			}
		}
	}
	etag := ids.New()
	data, _ := json.Marshal(input)
	if create {
		var count int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM olp.workload_issuers").Scan(&count); e != nil {
			return "", e
		}
		if count >= 64 {
			return "", httpkit.Invalid("issuer", "At most 64 workload issuers can be registered.")
		}
		_, e = tx.Exec(ctx, "INSERT INTO olp.workload_issuers(id,issuer,document,created_by,etag) VALUES($1,$2,$3,$4,$5)", id, input.Issuer, data, p.UserID(), etag)
	} else {
		_, e = tx.Exec(ctx, "UPDATE olp.workload_issuers SET document=$2,etag=$3,updated_at=now() WHERE id=$1", id, data, etag)
	}
	if e != nil {
		return "", e
	}
	if _, e = access.AdvanceAuthority(ctx, tx); e != nil {
		return "", e
	}
	return etag, nil
}

// ValidateWorkloadBindings preserves enrolled subjects during planning and writes.
func ValidateWorkloadBindings(ctx context.Context, q database.Queryer, id string, input workload.Config) error {
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
			return httpkit.Fail(409, "workload_mapping_in_use", "A registered principal's mapping and project cannot be removed or reassigned; disable the issuer to stop access.")
		}
	}
	return nil
}
