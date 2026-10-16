package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/secrets"
)

type memory struct {
	mu            sync.Mutex
	values        map[string]string
	fail          bool
	searchResults []any
}

func (m *memory) Do(_ context.Context, a ...string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return nil, errors.New("offline")
	}
	switch a[0] {
	case "FT.CREATE":
		return "OK", nil
	case "FT.SEARCH":
		return m.searchResults, nil
	case "SET":
		m.values[a[1]] = a[2]
		return "OK", nil
	case "HGET":
		v, ok := m.values[a[1]]
		if !ok {
			return nil, nil
		}
		return v, nil
	case "EVAL":
		if a[2] == "2" {
			out := []any{}
			for i := range 2 {
				if m.values[a[3+i]] == "" {
					m.values[a[3+i]] = a[5+i]
				}
				out = append(out, m.values[a[3+i]])
			}
			key := a[7] + ":" + out[0].(string) + ":" + out[1].(string)
			bucket := a[8] + ":" + out[0].(string) + ":" + out[1].(string)
			value := ""
			if a[9] == "1" {
				value = m.values[key]
			}
			return []any{key, bucket, value}, nil
		}
		m.values[a[3]] = a[4]
		return int64(1), nil
	}
	return nil, errors.New("unsupported")
}
func TestSemanticCandidatesAuthenticateNamespace(t *testing.T) {
	for _, change := range []string{"same scope", "embedding model", "project", "route", "key", "revision", "project purge", "route purge"} {
		t.Run(change, func(t *testing.T) {
			s, m := testStore(t)
			space := Space{Project: "project", Route: "route", Scope: "key", Fingerprint: "revision"}
			original, err := s.Begin(t.Context(), space, []byte("original"))
			if err != nil {
				t.Fatal(err)
			}
			original.VectorSpace = "embedding-model-v1"
			p := Policy{Mode: "semantic", AllowUnsealedVectors: true, EmbeddingRoute: "embeddings"}
			if err := s.Put(t.Context(), original, Entry{Body: []byte("private response")}, p, Controls{}, []float32{1, 0}); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "project":
				space.Project = "other"
			case "route":
				space.Route = "other"
			case "key":
				space.Scope = "other"
			case "revision":
				space.Fingerprint = "other"
			case "project purge":
				err = s.Purge(t.Context(), "project", space.Project)
			case "route purge":
				err = s.Purge(t.Context(), "route", space.Route)
			}
			if err != nil {
				t.Fatal(err)
			}
			query, err := s.Begin(t.Context(), space, []byte("similar"))
			if err != nil {
				t.Fatal(err)
			}
			query.VectorSpace = original.VectorSpace
			if change == "embedding model" {
				query.VectorSpace = "embedding-model-v2"
			}
			// Simulate an altered search result pointing to a valid ciphertext
			// from a different caller, revision, or purged namespace.
			m.searchResults = []any{int64(1), original.Key, []any{"__vector_score", "0"}}
			entry, _, err := s.Nearest(t.Context(), query, []float32{1, 0}, p, Controls{})
			if change == "same scope" {
				if err != nil || entry == nil || string(entry.Body) != "private response" {
					t.Fatalf("valid semantic candidate rejected: %v %v", entry, err)
				}
			} else if entry != nil || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("foreign semantic candidate accepted: %v %v", entry, err)
			}
		})
	}
}

func TestUnscopedSealedEntriesAreRejected(t *testing.T) {
	s, m := testStore(t)
	ticket, err := s.Begin(t.Context(), Space{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Older ciphertext authenticated only the entry key. It cannot establish
	// that a semantic search candidate belongs to the requested namespace.
	plain, err := json.Marshal(Entry{CreatedAt: s.now(), ExpiresAt: s.now().Add(time.Hour), Body: []byte("unscoped")})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := s.Keys.Seal(s.Installation, secrets.ResponseCache, ticket.Key, plain)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(sealed{Version: s.Keys.Active, Data: ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	m.values[ticket.Key] = string(blob)
	if entry, err := s.Get(t.Context(), ticket, Controls{}); entry != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unscoped entry accepted: %v %v", entry, err)
	}
}
func testStore(t *testing.T) (*Store, *memory) {
	t.Helper()
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("1", 64) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := &memory{values: map[string]string{}}
	return &Store{Client: m, Keys: ring, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{2}, 32), "test"), Installation: "test"}, m
}
func TestSealedExpiryIsolationAndEviction(t *testing.T) {
	s, m := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	space := Space{Project: "p", Route: "r", Scope: "key1", Fingerprint: "revision1"}
	first, err := s.Begin(ctx, space, []byte(`{"input":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{Body: []byte("private answer"), Metadata: json.RawMessage(`{}`)}
	if err = s.Put(ctx, first, entry, Policy{Mode: "exact", TTLSeconds: 10}, Controls{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, v := range m.values {
		if strings.Contains(v, "private") {
			t.Fatal("cache exposed plaintext")
		}
	}
	if got, err := s.Get(ctx, first, Controls{}); err != nil || string(got.Body) != "private answer" {
		t.Fatal(got, err)
	}
	for _, next := range []Space{{Project: "p2", Route: "r", Scope: "key1", Fingerprint: "revision1"}, {Project: "p", Route: "r", Scope: "key2", Fingerprint: "revision1"}, {Project: "p", Route: "r", Scope: "key1", Fingerprint: "revision2"}} {
		ticket, _ := s.Begin(ctx, next, []byte(`{"input":"private"}`))
		if got, err := s.Get(ctx, ticket, Controls{}); err != nil || got != nil {
			t.Fatal("cross-scope/revision hit")
		}
	}
	if err = s.Purge(ctx, "route", "r"); err != nil {
		t.Fatal(err)
	}
	next, _ := s.Begin(ctx, space, []byte(`{"input":"private"}`))
	if next.Key == first.Key {
		t.Fatal("purge reused namespace")
	}
	delete(m.values, s.generationKey("route", "r"))
	after, _ := s.Begin(ctx, space, []byte(`{"input":"private"}`))
	if after.Key == first.Key || after.Key == next.Key {
		t.Fatal("eviction resurrected namespace")
	}
	now = now.Add(11 * time.Second)
	if got, err := s.Get(ctx, first, Controls{}); err != nil || got != nil {
		t.Fatal("expired response served")
	}
}
func TestTamperingAndStoreControls(t *testing.T) {
	s, m := testStore(t)
	ctx := t.Context()
	key, _ := s.Begin(ctx, Space{}, nil)
	p := Policy{Mode: "exact"}
	e := Entry{Body: []byte("answer"), Metadata: json.RawMessage(`{}`)}
	if err := s.Put(ctx, key, e, p, Controls{NoStore: true}, nil); !errors.Is(err, ErrNotStored) {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, key, Controls{}); got != nil {
		t.Fatal("no-store wrote a response")
	}
	if err := s.Put(ctx, key, e, p, Controls{}, nil); err != nil {
		t.Fatal(err)
	}
	var blob sealed
	_ = json.Unmarshal([]byte(m.values[key.Key]), &blob)
	blob.Data[len(blob.Data)-1] ^= 1
	raw, _ := json.Marshal(blob)
	m.values[key.Key] = string(raw)
	if _, err := s.Get(ctx, key, Controls{}); err == nil {
		t.Fatal("accepted changed ciphertext")
	}
	m.fail = true
	if _, err := s.Begin(ctx, Space{}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
func TestCanonicalPrecisionAndControls(t *testing.T) {
	parse := func(s string) []byte {
		d, err := oif.ParseJSON([]byte(s), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		return Canonical(d)
	}
	if !bytes.Equal(parse(`{"b":9007199254740993,"a":"\u0061"}`), parse(`{ "a":"a", "b":9007199254740993 }`)) {
		t.Fatal("equivalent source differs")
	}
	if bytes.Equal(parse(`{"n":9007199254740992}`), parse(`{"n":9007199254740993}`)) {
		t.Fatal("lost numeric precision")
	}
	for _, raw := range []string{"ttl=-1", "max-age=1, max-age=2", "no-store=true", "bogus", "ttl=9999999"} {
		if _, err := ParseControls(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	c, err := ParseControls("no-cache, ttl=3, max-age=4")
	if err != nil || !c.NoCache || *c.TTL != 3 || *c.MaxAge != 4 {
		t.Fatal(c, err)
	}
}
func TestPolicySafety(t *testing.T) {
	for _, p := range []Policy{{Mode: "exact", Scope: "all"}, {Mode: "semantic", EmbeddingRoute: "embeddings"}, {Mode: "exact", MaxEntryBytes: -1}, {Mode: "semantic", AllowUnsealedVectors: true, EmbeddingRoute: "embeddings", SimilarityThreshold: 1.1}} {
		if p.Validate() == nil {
			t.Fatal("accepted invalid policy", p)
		}
	}
	p := Policy{Mode: "semantic", AllowUnsealedVectors: true, EmbeddingRoute: "embeddings"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}
