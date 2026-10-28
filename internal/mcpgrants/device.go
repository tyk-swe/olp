package mcpgrants

import (
	"context"
	"errors"
	"time"

	"golang.org/x/oauth2"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
)

func (s *Store) Poll(ctx context.Context, id, member string) (Authorization, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Authorization{}, err
	}
	defer tx.Rollback(ctx)
	item, readErr := readFlow(ctx, tx, id, member)
	if readErr != nil {
		return Authorization{}, readErr
	}
	if item.Outcome == "completed" {
		return Authorization{State: Active}, nil
	}
	if item.Outcome != "" {
		return Authorization{State: StandingState(item.Outcome)}, nil
	}
	if item.Flow != DeviceCode {
		return Authorization{}, access.Invalid("enrollment_id", "This enrollment does not use device authorization.")
	}
	attempt := access.NewID()
	tag, claimErr := tx.Exec(ctx, `UPDATE olp.mcp_grant_enrollments SET poll_at=now()+$3::integer*interval '1 second',exchange_attempt_id=$2 WHERE id=$1 AND poll_at<=now() AND exchange_attempt_id IS NULL AND outcome IS NULL`, id, attempt, int64(grants.PollLease/time.Second))
	if claimErr != nil {
		return Authorization{}, claimErr
	}
	if tag.RowsAffected() != 1 {
		return Authorization{State: Pending}, nil
	}
	session, sessionErr := s.loadSession(ctx, tx, item)
	if sessionErr != nil {
		return Authorization{}, sessionErr
	}
	if _, err := liveBinding(ctx, tx, item.Binding); err != nil {
		return Authorization{}, err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return Authorization{}, commitErr
	}
	client, clientErr := s.oauthClient(item.Binding, "")
	if clientErr != nil {
		return Authorization{}, clientErr
	}
	defer client.client.CloseIdleConnections()
	token, pollErr := client.config.Exchange(client.context(ctx), "", oauth2.SetAuthURLParam("grant_type", "urn:ietf:params:oauth:grant-type:device_code"), oauth2.SetAuthURLParam("device_code", session.DeviceCode))
	if pollErr == nil {
		if err := s.completeGrant(ctx, item, attempt, token); err != nil {
			return Authorization{}, err
		}
		return Authorization{State: Active}, nil
	}
	if reported, ok := errors.AsType[*oauth2.RetrieveError](pollErr); ok {
		switch reported.ErrorCode {
		case "authorization_pending", "slow_down":
			interval := grants.DevicePollInterval(time.Duration(item.Interval)*time.Second, reported.ErrorCode == "slow_down")
			_, updateErr := s.Pool.Exec(ctx, `UPDATE olp.mcp_grant_enrollments SET exchange_attempt_id=NULL,poll_interval=$3,poll_at=now()+$3::integer*interval '1 second' WHERE id=$1 AND exchange_attempt_id=$2 AND outcome IS NULL`, id, attempt, int64(interval/time.Second))
			return Authorization{State: Pending}, updateErr
		case "access_denied":
			return Authorization{State: Denied}, s.endFlow(ctx, id, "denied")
		case "expired_token":
			return Authorization{State: Expired}, s.endFlow(ctx, id, "expired")
		}
	}
	return Authorization{}, errors.Join(errOAuth, s.endFlow(ctx, id, "failed"))
}
