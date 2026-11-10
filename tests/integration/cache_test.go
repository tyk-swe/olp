//go:build integration

package integration_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/tests/harness"
)

var cachePolicy = cache.Policy{Mode: "exact", TTLSeconds: 60}

func keyRing(t *testing.T, key string) *secrets.KeyRing {
	t.Helper()
	ring, err := secrets.ParseRing(fmt.Appendf(nil, `{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat(key, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func newCache(t *testing.T) *cache.Store {
	t.Helper()
	installation := uuid.NewString()
	return &cache.Store{
		Client: harness.Valkey(t), Keys: keyRing(t, "01"), Installation: installation,
		Auth: secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), installation),
	}
}

func newSpace() cache.Space {
	return cache.Space{Project: uuid.NewString(), Route: uuid.NewString(), NamedRoute: uuid.NewString(), Scope: "key", Fingerprint: "caller"}
}

func remember(t *testing.T, store *cache.Store, space cache.Space, request, answer string) cache.Ticket {
	t.Helper()
	ticket, err := store.Begin(t.Context(), space, []byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), ticket, cache.Entry{Body: []byte(answer)}, cachePolicy, cache.Controls{}, nil); err != nil {
		t.Fatal(err)
	}
	return ticket
}

func recall(t *testing.T, store *cache.Store, space cache.Space, request string, controls cache.Controls) string {
	t.Helper()
	_, entry, err := store.Lookup(t.Context(), space, []byte(request), controls)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		return ""
	}
	return string(entry.Body)
}

func TestResponseCacheAnswersOnlyTheSpaceAndRequestThatStoredIt(t *testing.T) {
	t.Parallel()
	store := newCache(t)
	space := newSpace()
	remember(t, store, space, "request", "answer")
	if got := recall(t, store, space, "request", cache.Controls{}); got != "answer" {
		t.Fatalf("got %q for the stored request, want its answer", got)
	}
	otherInstallation := *store
	otherInstallation.Installation = uuid.NewString()
	for name, got := range map[string]string{
		"another request":      recall(t, store, space, "another request", cache.Controls{}),
		"another project":      recall(t, store, cache.Space{Project: uuid.NewString(), Route: space.Route, NamedRoute: space.NamedRoute, Scope: space.Scope, Fingerprint: space.Fingerprint}, "request", cache.Controls{}),
		"another route":        recall(t, store, cache.Space{Project: space.Project, Route: uuid.NewString(), NamedRoute: space.NamedRoute, Scope: space.Scope, Fingerprint: space.Fingerprint}, "request", cache.Controls{}),
		"another caller":       recall(t, store, cache.Space{Project: space.Project, Route: space.Route, NamedRoute: space.NamedRoute, Scope: space.Scope, Fingerprint: "another caller"}, "request", cache.Controls{}),
		"another installation": recall(t, &otherInstallation, space, "request", cache.Controls{}),
	} {
		if got != "" {
			t.Errorf("%s was answered %q from the cache", name, got)
		}
	}
}

func TestResponseCacheStoresNothingTheCallerOrPolicyRefuses(t *testing.T) {
	t.Parallel()
	store := newCache(t)
	for name, put := range map[string]struct {
		policy   cache.Policy
		controls cache.Controls
	}{
		"no-store":                     {cachePolicy, cache.Controls{NoStore: true}},
		"a zero lifetime":              {cachePolicy, cache.Controls{TTL: new(0)}},
		"an entry over the size limit": {cache.Policy{Mode: "exact", TTLSeconds: 60, MaxEntryBytes: 64}, cache.Controls{}},
	} {
		space := newSpace()
		ticket, err := store.Begin(t.Context(), space, []byte("request"))
		if err != nil {
			t.Fatal(err)
		}
		entry := cache.Entry{Body: []byte(strings.Repeat("answer", 32))}
		if err := store.Put(t.Context(), ticket, entry, put.policy, put.controls, nil); !errors.Is(err, cache.ErrNotStored) {
			t.Errorf("%s: got %v, want %v", name, err, cache.ErrNotStored)
		}
		if got := recall(t, store, space, "request", cache.Controls{}); got != "" {
			t.Errorf("%s: the refused entry was answered from the cache", name)
		}
	}
}

func TestResponseCacheStopsAnsweringWhenAnEntryIsTooOld(t *testing.T) {
	t.Parallel()
	store := newCache(t)
	space := newSpace()
	remember(t, store, space, "request", "answer")
	after := func(age time.Duration) *cache.Store {
		later := *store
		later.Now = func() time.Time { return time.Now().Add(age) }
		return &later
	}
	if got := recall(t, after(30*time.Second), space, "request", cache.Controls{MaxAge: new(40)}); got != "answer" {
		t.Errorf("got %q for a 30 second old entry under a 40 second max-age, want its answer", got)
	}
	if got := recall(t, after(30*time.Second), space, "request", cache.Controls{MaxAge: new(10)}); got != "" {
		t.Error("a 30 second old entry was answered under a 10 second max-age")
	}
	if got := recall(t, after(61*time.Second), space, "request", cache.Controls{}); got != "" {
		t.Error("an entry was answered after its 60 second lifetime")
	}
	expiring := newSpace()
	ticket := remember(t, store, expiring, "request", "answer")
	lifetime, err := store.Client.Do(t.Context(), "TTL", ticket.Key)
	if seconds, ok := lifetime.(int64); err != nil || !ok || seconds < 1 || seconds > int64(cachePolicy.TTLSeconds) {
		t.Errorf("got a lifetime of %v (%v) in Valkey, want between 1 and %d seconds", lifetime, err, cachePolicy.TTLSeconds)
	}
}

func TestPurgeMakesStoredEntriesUnreachable(t *testing.T) {
	t.Parallel()
	store := newCache(t)
	space := newSpace()
	for name, purge := range map[string]struct{ kind, id string }{
		"the project":     {"project", space.Project},
		"the route":       {"route", space.Route},
		"the named route": {"route", space.NamedRoute},
	} {
		remember(t, store, space, "request", "answer")
		if err := store.Purge(t.Context(), "project", uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if got := recall(t, store, space, "request", cache.Controls{}); got != "answer" {
			t.Fatalf("got %q after purging an unrelated project, want the stored answer", got)
		}
		if err := store.Purge(t.Context(), purge.kind, purge.id); err != nil {
			t.Fatal(err)
		}
		if got := recall(t, store, space, "request", cache.Controls{}); got != "" {
			t.Errorf("purging %s left the entry answering %q", name, got)
		}
	}
}

func TestResponseCacheRefusesEntriesItDidNotSealForThatRequest(t *testing.T) {
	t.Parallel()
	store := newCache(t)
	space := newSpace()
	source := remember(t, store, space, "request", "answer")
	sealed, err := store.Client.Do(t.Context(), "HGET", source.Key, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Begin(t.Context(), space, []byte("another request"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Client.Do(t.Context(), "HSET", target.Key, "sealed", sealed.(string)); err != nil {
		t.Fatal(err)
	}
	if _, entry, err := store.Lookup(t.Context(), space, []byte("another request"), cache.Controls{}); entry != nil || !errors.Is(err, cache.ErrUnavailable) {
		t.Errorf("got entry %v and %v for an entry copied from another request, want %v", entry, err, cache.ErrUnavailable)
	}
	rotated := *store
	rotated.Keys = keyRing(t, "02")
	if _, entry, err := rotated.Lookup(t.Context(), space, []byte("request"), cache.Controls{}); entry != nil || !errors.Is(err, cache.ErrUnavailable) {
		t.Errorf("got entry %v and %v under a different master key, want %v", entry, err, cache.ErrUnavailable)
	}
}
