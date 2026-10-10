package runtime

import (
	"context"
	"errors"
)

// CheckRouteFidelity fences a strict admission against the published route.
// Release installation is asynchronous and can fail independently of authority
// refresh. A last-known-good strict reader must not bypass a newer transformed
// revision's redaction policy. Requests admitted before publication retain their
// pinned contract; each new admission checks the database and fails closed.
func (m *Manager) CheckRouteFidelity(ctx context.Context, route Route) error {
	if !route.Fidelity.Strict() {
		return nil
	}
	var allowed bool
	err := m.pool.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id
 WHERE r.id=$1 AND r.slug=$2 AND r.state='active' AND v.fidelity->>'mode'='strict'
 )`, route.ID, route.Slug).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("published route no longer permits strict admission")
	}
	return nil
}
