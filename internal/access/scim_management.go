package access

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/scim"
)

const scimGroupJSON = `jsonb_build_object('id',g.id,'display_name',g.document->>'displayName','external_id',g.document->>'externalId','mapping',g.document->'` + scim.GroupExtension + `','etag',g.etag,'member_count',(SELECT count(*) FROM olp.scim_group_members m JOIN olp.users u ON u.id=m.user_id WHERE m.group_id=g.id AND u.role_management='provisioned'))`

func (s *Server) scimGroups(r *http.Request, _ Principal) (Reply, error) {
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+scimGroupJSON+" FROM olp.scim_groups g WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) scimGroupMapping(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "scim_id")
	if err != nil {
		return Reply{}, err
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
	if err = DecodeUnique(r, &input, 64<<10); err != nil {
		return Reply{}, err
	}
	if input.Mapping == nil {
		return Reply{}, Invalid("mapping", "Provide an explicit mapping object.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	document, etag, err := s.scimRead(r, tx, true, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	data, _ := json.Marshal(input.Mapping)
	var mapping map[string]any
	if err = json.Unmarshal(data, &mapping); err != nil {
		return Reply{}, err
	}
	document[scim.GroupExtension] = mapping
	affected, err := s.writeSCIMGroup(r, tx, id, document, "PUT")
	if err != nil {
		return Reply{}, err
	}
	if err = s.reconcileSCIMUsers(r, tx, p, affected); err != nil {
		return Reply{}, err
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "scim.group.mapping.update", "scim_group", id, "success"); err != nil {
		return Reply{}, err
	}
	var body []byte
	if err = tx.QueryRow(r.Context(), "SELECT "+scimGroupJSON+",g.etag::text FROM olp.scim_groups g WHERE id=$1", id).Scan(&body, &etag); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(body), etag))
}

// StoreSCIMMapping promotes policy only. Directory identities and memberships
// stay local; an absent named group is created with no members.
func (s *Server) StoreSCIMMapping(ctx context.Context, tx pgx.Tx, p Principal, name string, mapping map[string]any) error {
	if err := p.Authorize(Access); err != nil {
		return err
	}
	if ValidText("name", name, 100) != nil {
		return Invalid("scim_group_mappings", "Choose a nonempty group name up to 100 characters.")
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
		id = NewID()
		method = "POST"
		document = scim.Document{"schemas": []any{scim.GroupSchema}, "displayName": name}
	} else {
		document, _, err = s.scimRead(r, tx, true, id)
		if err != nil {
			return err
		}
	}
	document[scim.GroupExtension] = mapping
	affected, err := s.writeSCIMGroup(r, tx, id, document, method)
	if err != nil {
		return err
	}
	if err = s.reconcileSCIMUsers(r, tx, p, affected); err != nil {
		return err
	}
	if err = s.usableOwner(r, tx); err != nil {
		return err
	}
	_, err = AdvanceAuthority(ctx, tx)
	return err
}
