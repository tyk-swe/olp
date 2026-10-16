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
	mu     sync.Mutex
	values map[string]string
	fail   bool
}

func (m *memory) Do(_ context.Context, a ...string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return nil, errors.New("offline")
	}
	switch a[0] {
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
