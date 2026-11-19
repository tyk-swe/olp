package access

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

// ConsumeRecent spends the recent authentication the principal's session
// holds for purpose and resource, refusing with 428 reauthentication_required
// unless the session reauthenticated for them within five minutes.
func (s *Server) ConsumeRecent(r *http.Request, tx pgx.Tx, p authz.Principal, purpose, resource string) error {
	var target any
	if resource != "" {
		target = resource
	}
	tag, err := tx.Exec(r.Context(), "DELETE FROM olp.recent_auth WHERE digest=$1 AND session_id=$2 AND purpose=$3 AND resource_id IS NOT DISTINCT FROM $4::uuid AND expires_at>now()", s.Auth.Digest(secrets.RecentAuthDigest, CookieValue(r, RecentCookie)), p.SessionID, purpose, target)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return httpkit.Fail(428, "reauthentication_required", "Confirm your identity again before this operation.")
	}
	return nil
}
