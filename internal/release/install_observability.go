package release

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func (m *Manager) recordInstallStatus(ctx context.Context) error {
	desired, installed, failed := m.installStatus()
	_, err := m.pool.Exec(ctx, recordInstallStatusSQL, m.instanceID, desired, installed, failed)
	return err
}

func (m *Manager) installStatus() (int64, int64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	desired, installed := m.desired.Load(), m.release.Sequence
	return desired, installed, desired > installed && m.failed == desired
}

func ExpireInstallStatus(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, expireInstallStatusSQL)
	return err
}
