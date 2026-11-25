package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

const scimGroupJSON = `jsonb_build_object('id',g.id,'display_name',g.document->>'displayName','external_id',g.document->>'externalId','mapping',g.document->'` + scim.GroupExtension + `','etag',g.etag,'member_count',(SELECT count(*) FROM olp.scim_group_members m JOIN olp.users u ON u.id=m.user_id WHERE m.group_id=g.id AND u.role_management='provisioned'))`

func (s *Server) scimGroups(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+scimGroupJSON+" FROM olp.scim_groups g WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, page), err
}

func (s *Server) scimGroupMapping(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "scim_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Mapping *struct {
			Role     *string `json:"role"`
			Scope    string  `json:"accessScope"`
			Projects []struct {
				Value string `json:"value"`
				Role  string `json:"role"`
			} `json:"projects"`
		} `json:"mapping"`
	}
	if err = httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Mapping == nil {
		return httpkit.Reply{}, httpkit.Invalid("mapping", "Provide an explicit mapping object.")
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		document, etag, err := s.scimRead(r, tx, true, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		data, _ := json.Marshal(input.Mapping)
		var mapping map[string]any
		if err = json.Unmarshal(data, &mapping); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		document[scim.GroupExtension] = mapping
		managed, err := scimManagedProjects(r, tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		affected, err := s.writeSCIMGroup(r, tx, id, document, "PUT")
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = s.reconcileSCIMUsers(r, tx, p, affected); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = keepSCIMManagers(r, tx, managed); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var body []byte
		if err = tx.QueryRow(r.Context(), "SELECT "+scimGroupJSON+",g.etag::text FROM olp.scim_groups g WHERE id=$1", id).Scan(&body, &etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(json.RawMessage(body), etag), access.Event{Action: "scim.group.mapping.update", Resource: "scim_group", ID: id}, nil
	})
}

// keepSCIMManagers refuses a mapping change that leaves a project it managed
// through SCIM without a delegated manager, as member changes are refused.
func keepSCIMManagers(r *http.Request, tx pgx.Tx, projects []string) error {
	orphaned, err := scimOrphansProject(r, tx, projects)
	if err == nil && orphaned {
		err = httpkit.Fail(409, "last_project_manager", "Keep at least one project manager.")
	}
	return err
}

// StoreSCIMMapping promotes policy only. Directory identities and memberships
// stay local; an absent named group is created with no members.
func (s *Server) StoreSCIMMapping(ctx context.Context, tx pgx.Tx, p authz.Principal, name string, mapping map[string]any) error {
	if err := p.Authorize(authz.Access); err != nil {
		return err
	}
	if httpkit.ValidText("name", name, 100) != nil {
		return httpkit.Invalid("scim_group_mappings", "Choose a nonempty group name up to 100 characters.")
	}
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM olp.scim_groups WHERE lower(document->>'displayName')=lower($1) FOR UPDATE", name).Scan(&id)
	create := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !create {
		return err
	}
	r := (&http.Request{}).WithContext(ctx)
	var document scim.Document
	method := "PUT"
	if create {
		id = ids.New()
		method = "POST"
		document = scim.Document{"schemas": []any{scim.GroupSchema}, "displayName": name}
	} else {
		document, _, err = s.scimRead(r, tx, true, id)
		if err != nil {
			return err
		}
	}
	document[scim.GroupExtension] = mapping
	managed, err := scimManagedProjects(r, tx)
	if err != nil {
		return err
	}
	affected, err := s.writeSCIMGroup(r, tx, id, document, method)
	if err != nil {
		return err
	}
	if err = s.reconcileSCIMUsers(r, tx, p, affected); err != nil {
		return err
	}
	if err = keepSCIMManagers(r, tx, managed); err != nil {
		return err
	}
	if err = EnsureUsableOwner(ctx, tx, s.access.LocalLoginDisabled()); err != nil {
		return err
	}
	_, err = access.AdvanceAuthority(ctx, tx)
	return err
}
