package runtime

import "context"

func (m *Manager) RecordInstallStatus(ctx context.Context, instance string) error {
	if instance == "" {
		return nil
	}
	desired, installed, failed := m.installStatus()
	_, err := m.pool.Exec(ctx, recordInstallStatusSQL, instance, desired, installed, failed)
	return err
}

func (m *Manager) installStatus() (int64, int64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	desired, installed := m.desired.Load(), m.release.Sequence
	return desired, installed, desired > installed && m.failed == desired
}
