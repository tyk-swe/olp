package identity

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type invitationRecord struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	Role            string     `json:"role"`
	InvitedBy       string     `json:"invited_by"`
	InvitedByEmail  *string    `json:"invited_by_email"`
	AcceptedByEmail *string    `json:"accepted_by_email"`
	RevokedByEmail  *string    `json:"revoked_by_email"`
	AcceptedAt      *time.Time `json:"accepted_at"`
	RevokedAt       *time.Time `json:"revoked_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	CreatedAt       time.Time  `json:"created_at"`
	Status          string     `json:"status"`
}

type createInvitationResponse struct {
	Invitation invitationRecord `json:"invitation"`
	Token      string           `json:"token"`
}

const invitationColumns = `i.id::text,i.email,i.role,i.invited_by::text,inviter.email,accepted.email,revoker.email,i.accepted_at,i.revoked_at,i.expires_at,i.created_at,CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END`
const invitationFrom = ` FROM olp.invitations i LEFT JOIN olp.users inviter ON inviter.id=i.invited_by LEFT JOIN olp.users accepted ON accepted.id=i.accepted_by LEFT JOIN olp.users revoker ON revoker.id=i.revoked_by`

func scanInvitation(row pgx.Row) (invitationRecord, error) {
	var rec invitationRecord
	err := row.Scan(
		&rec.ID,
		&rec.Email,
		&rec.Role,
		&rec.InvitedBy,
		&rec.InvitedByEmail,
		&rec.AcceptedByEmail,
		&rec.RevokedByEmail,
		&rec.AcceptedAt,
		&rec.RevokedAt,
		&rec.ExpiresAt,
		&rec.CreatedAt,
		&rec.Status,
	)
	return rec, err
}

func (s *accountService) invitations(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+invitationColumns+invitationFrom+" WHERE i.id<$1 ORDER BY i.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []invitationRecord{}
	for rows.Next() {
		rec, err := scanInvitation(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, p, func(rec invitationRecord) string { return rec.ID }), nil
}

func invitation(r *http.Request, tx pgx.Tx, id string) (invitationRecord, error) {
	return scanInvitation(tx.QueryRow(r.Context(), "SELECT "+invitationColumns+invitationFrom+" WHERE i.id=$1", id))
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
		return httpkit.Reply{}, problem.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	hours := 168
	if input.Hours != nil {
		hours = *input.Hours
	}
	if hours < 1 || hours > 720 {
		return httpkit.Reply{}, problem.Invalid("expires_in_hours", "Use 1–720 hours.")
	}
	return access.Mutate(s.access, r, input, func(tx pgx.Tx, _ authz.Principal) (struct{}, error) {
		return struct{}{}, s.requireInvitationLogin(r, tx)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var exists bool
		if err := tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&exists); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if exists {
			return httpkit.Reply{}, access.Event{}, problem.Fail(409, "user_exists", "This email already belongs to a member.")
		}
		id, token := ids.New(), secrets.Token()
		if _, err := tx.Exec(r.Context(), "UPDATE olp.invitations SET revoked_at=now(),revoked_by=$2 WHERE email=$1 AND accepted_at IS NULL AND revoked_at IS NULL", address, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "INSERT INTO olp.invitations(id,email,role,digest,invited_by,expires_at) VALUES($1,$2,$3,$4,$5,$6)", id, address, input.Role, s.access.Auth.Digest(secrets.InvitationDigest, token), p.UserID(), time.Now().Add(time.Duration(hours)*time.Hour)); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		record, err := invitation(r, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Reply{
			Status:   201,
			Body:     createInvitationResponse{Invitation: record, Token: token},
			Location: "/api/v1/invitations/" + id,
		}
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
			return httpkit.Reply{}, access.Event{}, problem.Fail(409, "invitation_accepted", "An accepted invitation cannot be revoked.")
		}
		if revoked != nil {
			record, err := invitation(r, tx, id)
			return httpkit.OK(record), access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.invitations SET revoked_at=now(),revoked_by=$2 WHERE id=$1", id, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		record, err := invitation(r, tx, id)
		return httpkit.OK(record), access.Event{Action: "invitation.revoke", Resource: "invitation", ID: id}, err
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
	if err := textfield.Validate("display_name", input.DisplayName, 100); err != nil {
		return httpkit.Reply{}, err
	}
	var hash string
	if err := s.passwords.run(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return httpkit.Reply{}, err
	}
	displayName := strings.TrimSpace(input.DisplayName)
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		reply, err := s.completeInvitationAcceptance(r, tx, input.Token, displayName, hash)
		return reply, err == nil, err
	})
}

func (s *accountService) completeInvitationAcceptance(r *http.Request, tx pgx.Tx, token, displayName, hash string) (httpkit.Reply, error) {
	if err := s.requireInvitationLogin(r, tx); err != nil {
		return httpkit.Reply{}, err
	}
	var id, address, role string
	err := tx.QueryRow(r.Context(), "SELECT id::text,email,role FROM olp.invitations WHERE digest=$1 AND expires_at>now() AND accepted_at IS NULL AND revoked_at IS NULL", s.access.Auth.Digest(secrets.InvitationDigest, token)).Scan(&id, &address, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, problem.Fail(410, "invitation_invalid", "The invitation is expired, retired, or already used.")
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	userID := ids.New()
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,$5,$6)", userID, address, displayName, hash, role, ids.New()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "UPDATE olp.invitations SET accepted_at=now(),accepted_by=$2 WHERE id=$1", id, userID); err != nil {
		return httpkit.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(userID), "invitation.accept", "invitation", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return s.mfa.localSession(r, tx, userID)
}

func (s *accountService) requireInvitationLogin(r *http.Request, q database.Queryer) error {
	enabled, err := isLocalLoginEnabled(r.Context(), s.access, q)
	if err != nil {
		return err
	}
	if !enabled {
		return problem.Fail(409, "invitation_local_login_required", "Password invitations require local sign-in. Ask an owner to configure an OIDC role mapping, then sign in with single sign-on, or enable local sign-in before using this invitation.")
	}
	return nil
}
