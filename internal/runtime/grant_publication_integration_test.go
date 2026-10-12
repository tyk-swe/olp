//go:build integration

package runtime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

type grantReadBarrier struct {
	armed   atomic.Bool
	once    sync.Once
	started chan struct{}
	resume  chan struct{}
}

func (b *grantReadBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if b.armed.Load() && strings.HasPrefix(data.SQL, "SELECT credential_id::text,generation FROM olp.provider_grants") {
		b.once.Do(func() {
			close(b.started)
			select {
			case <-b.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (*grantReadBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestIntegrationOldMembershipPollCannotOverwritePublishedGrantSeeds(t *testing.T) {
	barrier := &grantReadBarrier{started: make(chan struct{}), resume: make(chan struct{})}
	pool := credentialPool(t, barrier)
	var resume sync.Once
	releasePoll := func() { resume.Do(func() { close(barrier.resume) }) }
	t.Cleanup(releasePoll)
	user, provider, old, replacement := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag)
		VALUES($1,'grant-publication@example.test','grant publication','owner',$2)`, user, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		VALUES($1,'grant publication','openai_compatible','draft','{}',$2,$3,$4)`, provider, uuid.NewString(), uuid.NewString(), user); err != nil {
		t.Fatal(err)
	}
	for i, credential := range []string{old, replacement} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_credentials(id,provider_id,version) VALUES($1,$2,$3)`, credential, provider, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, credential); err != nil {
			t.Fatal(err)
		}
	}
	m := credentialManager(t, time.Now())
	m.pool, m.keys = pool, &secrets.KeyRing{}
	publishGrantCandidate(t, m, grantCandidate(1, 1, old))
	barrier.armed.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	polled := make(chan error, 1)
	go func() { polled <- m.refreshGrants(ctx) }()
	select {
	case <-barrier.started:
	case err := <-polled:
		t.Fatal("old membership poll did not reach its coherent grant read", err)
	case <-ctx.Done():
		t.Fatal("old membership poll did not start", ctx.Err())
	}
	// The old membership is now captured, with the actual database read
	// explicitly stalled. Publication must wait for that poll's completion.
	type result struct {
		installed bool
		err       error
	}
	entering, published := make(chan struct{}), make(chan result, 1)
	go func() {
		close(entering)
		installed, err := m.publishRelease(ctx, grantCandidate(2, 1, replacement))
		published <- result{installed, err}
	}()
	<-entering
	select {
	case got := <-published:
		t.Fatal("release bypassed the old membership poll", got)
	case <-time.After(25 * time.Millisecond):
	}
	releasePoll()
	if err := <-polled; err != nil {
		t.Fatal(err)
	}
	if got := <-published; !got.installed || got.err != nil {
		t.Fatal("release did not publish after the old poll completed", got)
	}
	if m.Release().Sequence != 2 || m.GrantGeneration(old) != 0 || m.GrantGeneration(replacement) != 1 {
		t.Fatal("old poll erased replacement seeds or restored removed membership")
	}
	if err := m.refreshGrants(ctx); err != nil {
		t.Fatal("new membership poll did not reuse its coherent initial seed", err)
	}
	if m.GrantGeneration(old) != 0 || m.GrantGeneration(replacement) != 1 {
		t.Fatal("new poll used the previous release's membership")
	}
}
