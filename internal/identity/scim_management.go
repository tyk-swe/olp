package identity

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/identity/scim"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type scimGroupSummary struct {
	ID          string             `json:"id"`
	DisplayName string             `json:"display_name"`
	ExternalID  *string            `json:"external_id"`
	Mapping     SCIMGroupExtension `json:"mapping"`
	ETag        string             `json:"etag"`
	MemberCount int64              `json:"member_count"`
}

const scimGroupSummarySelect = `SELECT g.id::text,g.document->>'displayName',g.document->>'externalId',COALESCE(g.document->'` + scim.GroupExtension + `','{}'::jsonb),g.etag::text,(SELECT count(*) FROM olp.scim_group_members m JOIN olp.users u ON u.id=m.user_id WHERE m.group_id=g.id AND u.role_management='provisioned') FROM olp.scim_groups g`

func scanSCIMGroupSummary(row pgx.CollectableRow) (scimGroupSummary, error) {
	var summary scimGroupSummary
	if err := row.Scan(&summary.ID, &summary.DisplayName, &summary.ExternalID, &summary.Mapping, &summary.ETag, &summary.MemberCount); err != nil {
		return scimGroupSummary{}, err
	}
	if summary.Mapping.Projects == nil {
		summary.Mapping.Projects = []SCIMGroupProjectRef{}
	}
	return summary, nil
}

func readSCIMGroupSummary(ctx context.Context, q database.Queryer, id string) (scimGroupSummary, error) {
	var summary scimGroupSummary
	err := q.QueryRow(ctx, scimGroupSummarySelect+" WHERE g.id=$1", id).Scan(
		&summary.ID, &summary.DisplayName, &summary.ExternalID, &summary.Mapping, &summary.ETag, &summary.MemberCount,
	)
	if err != nil {
		return scimGroupSummary{}, err
	}
	if summary.Mapping.Projects == nil {
		summary.Mapping.Projects = []SCIMGroupProjectRef{}
	}
	return summary, nil
}

func readSCIMGroupHeader(ctx context.Context, q database.Queryer, id string) (string, string, string, error) {
	var displayName, externalID, etag string
	err := q.QueryRow(ctx, "SELECT g.document->>'displayName',COALESCE(g.document->>'externalId',''),g.etag::text FROM olp.scim_groups g WHERE g.id=$1", id).Scan(&displayName, &externalID, &etag)
	return displayName, externalID, etag, err
}

func readProvisionedGroupMembers(ctx context.Context, q database.Queryer, id string) ([]string, error) {
	rows, err := q.Query(ctx, "SELECT u.id::text FROM olp.scim_group_members m JOIN olp.users u ON u.id=m.user_id JOIN olp.scim_users su ON su.user_id=u.id WHERE m.group_id=$1 AND su.deleted_at IS NULL AND u.role_management='provisioned' ORDER BY u.id", id)
	if err != nil {
		return nil, err
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if members == nil {
		return []string{}, nil
	}
	return members, nil
}

func (s *scimService) scimGroups(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), scimGroupSummarySelect+" WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, scanSCIMGroupSummary)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(g scimGroupSummary) string { return g.ID }), nil
}

func (s *scimService) scimGroupMapping(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "scim_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Mapping *SCIMGroupExtension `json:"mapping"`
	}
	if err = httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Mapping == nil {
		return httpkit.Reply{}, problem.Invalid("mapping", "Provide an explicit mapping object.")
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		ctx := r.Context()
		displayName, externalID, etag, err := readSCIMGroupHeader(ctx, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		members, err := readProvisionedGroupMembers(ctx, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		extension, err := normalizeSCIMGroupExtension(ctx, tx, *input.Mapping)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		group := parsedSCIMGroup{display: displayName, external: externalID, extension: extension, members: members}
		if err = s.applyGroupMapping(ctx, tx, r, p, id, group, false); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		summary, err := readSCIMGroupSummary(ctx, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(summary, summary.ETag), access.Event{Action: "scim.group.mapping.update", Resource: "scim_group", ID: id}, nil
	})
}

func (s *Server) StoreSCIMMapping(ctx context.Context, tx pgx.Tx, p authz.Principal, name string, mapping SCIMGroupExtension) error {
	return s.scim.storeMapping(ctx, tx, p, name, mapping)
}

func (s *scimService) storeMapping(ctx context.Context, tx pgx.Tx, p authz.Principal, name string, mapping SCIMGroupExtension) error {
	if err := p.Authorize(authz.Access); err != nil {
		return err
	}
	if textfield.Validate("name", name, 100) != nil {
		return problem.Invalid("scim_group_mappings", "Choose a nonempty group name up to 100 characters.")
	}
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM olp.scim_groups WHERE lower(document->>'displayName')=lower($1) FOR UPDATE", name).Scan(&id)
	create := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !create {
		return err
	}
	extension, err := normalizeSCIMGroupExtension(ctx, tx, mapping)
	if err != nil {
		return err
	}
	var group parsedSCIMGroup
	if create {
		id = ids.New()
		group = parsedSCIMGroup{display: name, extension: extension, members: []string{}}
	} else {
		displayName, externalID, _, readErr := readSCIMGroupHeader(ctx, tx, id)
		if readErr != nil {
			return readErr
		}
		members, membersErr := readProvisionedGroupMembers(ctx, tx, id)
		if membersErr != nil {
			return membersErr
		}
		group = parsedSCIMGroup{display: displayName, external: externalID, extension: extension, members: members}
	}
	return s.applyGroupMapping(ctx, tx, nil, p, id, group, create)
}

func (s *scimService) applyGroupMapping(ctx context.Context, tx pgx.Tx, r *http.Request, p authz.Principal, id string, group parsedSCIMGroup, create bool) error {
	managed, err := scimManagedProjects(ctx, tx)
	if err != nil {
		return err
	}
	affected, err := s.saveSCIMGroup(ctx, tx, id, group, create)
	if err != nil {
		return err
	}
	if err = s.reconcileSCIMUsers(ctx, tx, r, p, affected); err != nil {
		return err
	}
	if err = keepSCIMManagers(ctx, tx, managed); err != nil {
		return err
	}
	if err = EnsureUsableOwner(ctx, tx, s.access.LocalLoginDisabled()); err != nil {
		return err
	}
	_, err = access.AdvanceAuthority(ctx, tx)
	return err
}
