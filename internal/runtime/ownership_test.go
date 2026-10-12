package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func TestReleaseFreezesCallerOwnedSnapshotAndCredentials(t *testing.T) {
	provider := Provider{ID: uuid.NewString(), Name: "original", Kind: "openai_compatible", Enabled: true, RevisionID: uuid.NewString(), Capabilities: []Capability{}, Limits: &Limits{MaxConcurrency: quotaLimit(2)}}
	snapshot := quotaSnapshot(provider)
	credential := []byte("original")
	credentials := map[string][]byte{"credential": credential}
	release, err := NewRelease("release", 1, snapshot, credentials)
	if err != nil {
		t.Fatal(err)
	}
	*provider.Limits.MaxConcurrency = 9
	delete(snapshot.Providers, provider.ID)
	credential[0] = 'x'
	delete(credentials, "credential")
	served := release.Snapshot.Providers[provider.ID]
	if served.Name != "original" || *served.Limits.MaxConcurrency != 2 {
		t.Fatal("caller changed the validated serving configuration")
	}
	read, ok := release.Credential("credential")
	if !ok || string(read) != "original" {
		t.Fatal("caller changed the installed credential")
	}
	clear(read)
	read, _ = release.Credential("credential")
	if string(read) != "original" {
		t.Fatal("fixture credential read changed serving storage")
	}
	digest, err := release.Snapshot.Digest()
	if err != nil || digest != release.Digest {
		t.Fatal("frozen release digest changed", err)
	}
}

func TestSlowRefreshDoesNotConsumeSiblingObligationsBudget(t *testing.T) {
	started := make(chan struct{})
	progress := make(chan time.Duration, 1)
	err := refreshObligations(t.Context(), 100*time.Millisecond,
		func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() },
		func(ctx context.Context) error {
			<-started
			deadline, _ := ctx.Deadline()
			progress <- time.Until(deadline)
			return nil
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("slow obligation did not expire", err)
	}
	if remaining := <-progress; remaining < 50*time.Millisecond {
		t.Fatalf("sibling obligation inherited exhausted budget: %v", remaining)
	}
}

func TestRuntimeStopCancelsTasksAndRefusesNewOwners(t *testing.T) {
	m := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.DiscardHandler))
	finished := make(chan struct{})
	m.launch(func(ctx context.Context) { <-ctx.Done(); close(finished) })
	if err := m.StopContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("runtime returned before its task completed")
	}
	if m.launch(func(context.Context) { t.Error("task started after shutdown") }) {
		t.Fatal("runtime accepted new ownership after stopping")
	}
}

func TestRuntimeStopDoesNotExtendDeadlineForUnresponsiveCallback(t *testing.T) {
	m := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.DiscardHandler))
	blocked := make(chan struct{})
	defer func() { close(blocked); m.Stop() }()
	m.launch(func(context.Context) { <-blocked })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.StopContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("runtime reported an unresponsive callback as joined", err)
	}
}

func TestExternalResolutionCannotOutliveObservedRevocation(t *testing.T) {
	m := credentialManager(t, time.Now())
	started, resume := make(chan struct{}), make(chan struct{})
	var returned atomic.Bool
	m.ExternalSecrets = externalResolveFunc(func(context.Context, secretstore.Reference) ([]byte, error) {
		close(started)
		<-resume
		returned.Store(true)
		return []byte("private"), nil
	})
	release := &Release{references: map[string]secretstore.Reference{"credential": externalReference("1")}}
	result := make(chan error, 1)
	go func() {
		secret, _, err := m.Secret(t.Context(), release, "credential")
		if secret != nil {
			t.Error("resolution returned a revoked secret")
		}
		result <- err
	}()
	<-started
	m.mu.Lock()
	m.authority.ineligible["credential"] = Revoked
	m.mu.Unlock()
	close(resume)
	if err := <-result; !errors.Is(err, ErrCredentialUnavailable) || !returned.Load() {
		t.Fatal("remote completion bypassed the revocation observation boundary", err)
	}
}
