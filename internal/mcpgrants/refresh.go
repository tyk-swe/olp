package mcpgrants

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
)

type storedGrant struct {
	ID        string
	AccessID  string
	RefreshID *string
	AttemptID *string
	Expires   time.Time
}

func readGrant(ctx context.Context, q access.Queryer, bound Binding) (storedGrant, error) {
	var grant storedGrant
	err := q.QueryRow(ctx, `SELECT id::text,access_secret_id::text,refresh_secret_id::text,refresh_attempt_id::text,expires_at FROM olp.mcp_user_grants WHERE binding_digest=$1 AND revoked_at IS NULL AND failure_code IS NULL AND access_secret_id IS NOT NULL`, bound.Digest()).Scan(&grant.ID, &grant.AccessID, &grant.RefreshID, &grant.AttemptID, &grant.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return grant, ErrRequired
	}
	return grant, err
}

func (s *Store) readAccess(ctx context.Context, q access.Queryer, bound Binding, grant storedGrant) ([]byte, error) {
	token, err := s.Keys.Read(ctx, q, s.Installation, grant.AccessID, bound.accessPurpose())
	if err != nil {
		return nil, err
	}
	if len(token) == 0 {
		return nil, ErrRequired
	}
	return token, nil
}

func (s *Store) resolveGrant(ctx context.Context, bound Binding, revisionID string) ([]byte, error) {
	current, err := readGrant(ctx, s.Pool, bound)
	if err != nil {
		return nil, err
	}
	if current.Expires.After(time.Now()) {
		return s.readAccess(ctx, s.Pool, bound, current)
	}
	if current.RefreshID == nil {
		return nil, ErrRequired
	}
	resource, acquireErr := s.Pool.Acquire(ctx)
	if acquireErr != nil {
		return nil, acquireErr
	}
	conn := resource.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	return grants.WithRefreshLock(ctx, conn, "mcp:"+current.ID, true, func() ([]byte, error) { return s.refreshLocked(ctx, conn, bound, current.ID, revisionID) })
}

func (s *Store) refreshLocked(ctx context.Context, conn *pgx.Conn, bound Binding, lockedID, revisionID string) ([]byte, error) {
	current, err := readGrant(ctx, conn, bound)
	if err != nil {
		return nil, err
	}
	if current.ID != lockedID {
		return nil, ErrRequired
	}
	if current.Expires.After(time.Now()) {
		return s.readAccess(ctx, conn, bound, current)
	}
	if current.RefreshID == nil {
		return nil, ErrRequired
	}
	if current.AttemptID != nil {
		return nil, errors.Join(ErrRequired, s.failRefresh(ctx, current.ID, *current.AttemptID))
	}
	refresh, readErr := s.Keys.Read(ctx, conn, s.Installation, *current.RefreshID, bound.refreshPurpose())
	if readErr != nil {
		return nil, readErr
	}
	defer clear(refresh)
	return s.replaceGrant(ctx, conn, bound, current, revisionID, func(client oauthClient) (*oauth2.Token, error) {
		return client.config.TokenSource(client.context(ctx), &oauth2.Token{RefreshToken: string(refresh)}).Token()
	})
}

func (s *Store) replaceGrant(ctx context.Context, conn *pgx.Conn, bound Binding, current storedGrant, revisionID string, exchange func(oauthClient) (*oauth2.Token, error)) ([]byte, error) {
	if current.AttemptID != nil {
		return nil, errors.Join(ErrRequired, s.failRefresh(ctx, current.ID, *current.AttemptID))
	}
	if err := refreshBinding(ctx, conn, bound, revisionID); err != nil {
		return nil, err
	}
	client, clientErr := s.oauthClient(ctx, bound, "", revisionID)
	if clientErr != nil {
		return nil, clientErr
	}
	defer client.client.CloseIdleConnections()
	attempt := access.NewID()
	tag, claimErr := conn.Exec(ctx, `UPDATE olp.mcp_user_grants SET refresh_attempt_id=$2 WHERE id=$1 AND refresh_attempt_id IS NULL AND revoked_at IS NULL`, current.ID, attempt)
	if claimErr != nil {
		return nil, claimErr
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrRequired
	}
	token, exchangeErr := exchange(client)
	if exchangeErr != nil {
		return nil, errors.Join(ErrRequired, s.failRefresh(ctx, current.ID, attempt))
	}
	if err := validToken(token); err != nil {
		return nil, errors.Join(ErrRequired, s.failRefresh(ctx, current.ID, attempt))
	}
	if err := s.saveRefresh(ctx, bound, current, attempt, token, revisionID); err != nil {
		return nil, err
	}
	return []byte(token.AccessToken), nil
}

func (s *Store) failRefresh(ctx context.Context, id, attempt string) error {
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.Pool.Exec(record, `UPDATE olp.mcp_user_grants SET failure_code='refresh_failed' WHERE id=$1 AND refresh_attempt_id=$2`, id, attempt)
	return err
}

func (s *Store) saveRefresh(ctx context.Context, bound Binding, current storedGrant, attempt string, token *oauth2.Token, revisionID string) error {
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := s.Pool.Begin(record)
	if err != nil {
		return err
	}
	defer tx.Rollback(record)
	if err := refreshBinding(record, tx, bound, revisionID); err != nil {
		return err
	}
	accessID := access.NewID()
	if err := s.Keys.Store(record, tx, s.Installation, accessID, bound.accessPurpose(), []byte(token.AccessToken), nil); err != nil {
		return err
	}
	refreshID, refreshErr := s.storeRefresh(record, tx, bound, token.RefreshToken)
	if refreshErr != nil {
		return refreshErr
	}
	tag, updateErr := tx.Exec(record, `UPDATE olp.mcp_user_grants SET access_secret_id=$3,refresh_secret_id=$4,expires_at=$5,refresh_attempt_id=NULL,failure_code=NULL WHERE id=$1 AND refresh_attempt_id=$2 AND revoked_at IS NULL`, current.ID, attempt, accessID, refreshID, token.Expiry)
	if updateErr != nil {
		return updateErr
	}
	if tag.RowsAffected() != 1 {
		return ErrRequired
	}
	if _, err := tx.Exec(record, `DELETE FROM olp.secrets WHERE id=ANY($1::uuid[])`, replacedSecretIDs(current)); err != nil {
		return err
	}
	return tx.Commit(record)
}

func refreshBinding(ctx context.Context, q access.Queryer, bound Binding, revisionID string) error {
	if revisionID != "" && bound.AgentID != "" {
		_, err := revisionAgentBinding(ctx, q, bound, revisionID)
		return err
	}
	_, err := liveBinding(ctx, q, bound)
	return err
}

func replacedSecretIDs(current storedGrant) []string {
	if current.RefreshID == nil {
		return []string{current.AccessID}
	}
	return []string{current.AccessID, *current.RefreshID}
}
