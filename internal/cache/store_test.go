package cache

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

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
		if a[2] != "1" {
			n, err := strconv.Atoi(a[2])
			if err != nil {
				return nil, err
			}
			out := []string{}
			for i := range n {
				if m.values[a[3+i]] == "" {
					m.values[a[3+i]] = a[3+n+i]
				}
				out = append(out, m.values[a[3+i]])
			}
			base := 3 + 2*n
			key := a[base] + ":" + strings.Join(out, ":")
			bucket := a[base+1] + ":" + strings.Join(out, ":")
			value := ""
			if a[base+2] == "1" {
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
	for _, change := range []string{"same scope", "embedding model", "project", "route", "named route", "key", "revision", "project purge", "route purge", "named route purge", "named route eviction"} {
		t.Run(change, func(t *testing.T) {
			s, m := testStore(t)
			space := Space{Project: "project", Route: "route", NamedRoute: "named", Scope: "key", Fingerprint: "revision"}
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
			case "named route":
				space.NamedRoute = "other"
			case "key":
				space.Scope = "other"
			case "revision":
				space.Fingerprint = "other"
			case "project purge":
				err = s.Purge(t.Context(), "project", space.Project)
			case "route purge":
				err = s.Purge(t.Context(), "route", space.Route)
			case "named route purge":
				err = s.Purge(t.Context(), "route", space.NamedRoute)
			case "named route eviction":
				delete(m.values, s.generationKey("route", space.NamedRoute))
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

// Older ciphertext authenticated only the entry key. It cannot establish
// that a semantic search candidate belongs to the requested namespace.

func testStore(t *testing.T) (*Store, *memory) {
	t.Helper()
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("1", 64) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := &memory{values: map[string]string{}}
	return &Store{Client: m, Keys: ring, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{2}, 32), "test"), Installation: "test"}, m
}
