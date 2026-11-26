package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const invitationJSON = `jsonb_build_object('id',i.id,'email',i.email,'role',i.role,'invited_by',i.invited_by,'invited_by_email',inviter.email,'accepted_by_email',accepted.email,'revoked_by_email',revoker.email,'accepted_at',i.accepted_at,'revoked_at',i.revoked_at,'expires_at',i.expires_at,'created_at',i.created_at,'status',CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END)`
const invitationFrom = ` FROM olp.invitations i LEFT JOIN olp.users inviter ON inviter.id=i.invited_by LEFT JOIN olp.users accepted ON accepted.id=i.accepted_by LEFT JOIN olp.users revoker ON revoker.id=i.revoked_by`

func (s *accountService) invitations(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+invitationJSON+invitationFrom+" WHERE i.id<$1 ORDER BY i.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, p), err
}

func invitation(r *http.Request, tx pgx.Tx, id string) (any, error) {
	var data []byte
	err := tx.QueryRow(r.Context(), "SELECT "+invitationJSON+invitationFrom+" WHERE i.id=$1", id).Scan(&data)
	return json.RawMessage(data), err
}

func (s *accountService) createInvitation(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
		Hours *int   `json:"expires_in_hours"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !authz.ValidRole(input.Role) {
		return httpkit.Reply{}, httpkit.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	hours := 168
	if input.Hours != nil {
		hours = *input.Hours
	}
	if hours < 1 || hours > 720 {
		return httpkit.Reply{}, httpkit.Invalid("expires_in_hours", "Use 1–720 hours.")
	}
	return access.Mutate(s.access, r, input, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		if err = s.requireInvitationLogin(r, tx); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var exists bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&exists); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if exists {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "user_exists", "This email already belongs to a member.")
		}
		id, token := ids.New(), secrets.Token()
		if _, err = tx.Exec(r.Context(), "UPDATE olp.invitations SET revoked_at=now(),revoked_by=$2 WHERE email=$1 AND accepted_at IS NULL AND revoked_at IS NULL", address, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.invitations(id,email,role,digest,invited_by,expires_at) VALUES($1,$2,$3,$4,$5,$6)", id, address, input.Role, s.access.Auth.Digest(secrets.InvitationDigest, token), p.UserID(), time.Now().Add(time.Duration(hours)*time.Hour)); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body, err := invitation(r, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Reply{Status: 201, Body: map[string]any{"invitation": body, "token": token}, Location: "/api/v1/invitations/" + id}
		return result, access.Event{Action: "invitation.create", Resource: "invitation", ID: id}, nil
	})
}

func (s *accountService) retireInvitation(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "invitation_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, nil, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var accepted, revoked *time.Time
		if err := tx.QueryRow(r.Context(), "SELECT accepted_at,revoked_at FROM olp.invitations WHERE id=$1", id).Scan(&accepted, &revoked); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if accepted != nil {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "invitation_accepted", "An accepted invitation cannot be revoked.")
		}
		if revoked != nil {
			body, err := invitation(r, tx, id)
			return httpkit.OK(body), access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.invitations SET revoked_at=now(),revoked_by=$2 WHERE id=$1", id, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body, err := invitation(r, tx, id)
		return httpkit.OK(body), access.Event{Action: "invitation.revoke", Resource: "invitation", ID: id}, err
	})
}

func (s *accountService) acceptInvitation(r *http.Request) (httpkit.Reply, error) {
	var input struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if err := admitAuth(s.access, r, "invitation", input.Token); err != nil {
		return httpkit.Reply{}, err
	}
	if err := password(input.Password); err != nil {
		return httpkit.Reply{}, err
	}
	if err := httpkit.ValidText("display_name", input.DisplayName, 100); err != nil {
		return httpkit.Reply{}, err
	}
	var hash string
	if err := s.passwords.run(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.access.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = s.requireInvitationLogin(r, tx); err != nil {
		return httpkit.Reply{}, err
	}
	var id, address, role string
	err = tx.QueryRow(r.Context(), "SELECT id::text,email,role FROM olp.invitations WHERE digest=$1 AND expires_at>now() AND accepted_at IS NULL AND revoked_at IS NULL", s.access.Auth.Digest(secrets.InvitationDigest, input.Token)).Scan(&id, &address, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, httpkit.Fail(410, "invitation_invalid", "The invitation is expired, retired, or already used.")
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	userID := ids.New()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,$5,$6)", userID, address, strings.TrimSpace(input.DisplayName), hash, role, ids.New()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.invitations SET accepted_at=now(),accepted_by=$2 WHERE id=$1", id, userID); err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, authz.UserActor(userID), "invitation.accept", "invitation", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	response, err := s.mfa.localSession(r, tx, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return response, tx.Commit(r.Context())
}
