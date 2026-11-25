package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/cachepolicy"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/platform/coordination"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

func canonical(t *testing.T, raw string) string {
	t.Helper()
	document, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return string(Canonical(document))
}

func TestCanonicalMatchesOnlyRequestsWithTheSameMeaning(t *testing.T) {
	const request = `{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":1}`
	for _, same := range []string{
		`{ "temperature" : 1, "messages" : [ { "content" : "a/b é", "role" : "user" } ], "model" : "m" }`,
		`{"model":"m","messages":[{"role":"user","content":"a\/b é"}],"temperature":1}`,
	} {
		if canonical(t, same) != canonical(t, request) {
			t.Errorf("%s\n got %s\nwant %s", same, canonical(t, same), canonical(t, request))
		}
	}
	for _, different := range []string{
		`{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":1.0}`,
		`{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":"1"}`,
		`{"model":"m","messages":[{"role":"user","content":"A/b é"}],"temperature":1}`,
		`{"model":"m","messages":[{"role":"user","content":"a/b é"},{"role":"user","content":"a/b é"}],"temperature":1}`,
		`{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":1,"stream":null}`,
		`{"model":"m","messages":[{"content":"user","role":"a/b é"}],"temperature":1}`,
	} {
		if canonical(t, different) == canonical(t, request) {
			t.Errorf("%s shares a cache entry with %s", different, request)
		}
	}
	if first, second := canonical(t, `{"a":[1,2]}`), canonical(t, `{"a":[12]}`); first == second {
		t.Errorf("two elements and their concatenation share the entry %s", first)
	}
}

type fakeCommander struct {
	values    map[string]string
	hashes    map[string]map[string]string
	searchErr error
}

func (f *fakeCommander) Search(_ context.Context, a ...string) ([]coordination.SearchDocument, error) {
	start := strings.Index(a[1], "{")
	end := strings.Index(a[1], "}")
	if start < 0 || end <= start {
		return nil, errors.New("unsupported query")
	}
	bucket := a[1][start+1 : end]
	var documents []coordination.SearchDocument
	for key, hash := range f.hashes {
		if hash["bucket"] == bucket && hash["vector"] != "" {
			documents = append(documents, coordination.SearchDocument{Key: key, Doc: map[string]string{"__vector_score": "0"}})
		}
	}
	return documents, f.searchErr
}

func (f *fakeCommander) Do(_ context.Context, a ...string) (any, error) {
	switch a[0] {
	case "EVAL":
		if a[1] == lookupScript {
			return f.lookup(a)
		}
		if a[1] == putScript {
			hash := map[string]string{"sealed": a[4], "bucket": a[6]}
			if a[7] != "" {
				hash["vector"] = a[7]
			}
			f.hashes[a[3]] = hash
			return int64(1), nil
		}
		return nil, errors.New("unsupported script")
	case "FT.CREATE":
		return "OK", nil
	case "HGET":
		if hash, ok := f.hashes[a[1]]; ok {
			if value, ok := hash[a[2]]; ok {
				return value, nil
			}
		}
		return nil, nil
	case "SET":
		f.values[a[1]] = a[2]
		return "OK", nil
	case "GET":
		if value, ok := f.values[a[1]]; ok {
			return value, nil
		}
		return nil, nil
	case "MGET":
		reply := make([]any, len(a)-1)
		for i, key := range a[1:] {
			if value, ok := f.values[key]; ok {
				reply[i] = value
			}
		}
		return reply, nil
	}
	return nil, errors.New("unsupported command")
}

func (f *fakeCommander) lookup(a []string) (any, error) {
	n, err := strconv.Atoi(a[2])
	if err != nil {
		return nil, err
	}
	keys := a[3 : 3+n]
	argv := a[3+n:]
	generations := make([]string, n)
	for i, key := range keys {
		if value, ok := f.values[key]; ok {
			generations[i] = value
		} else {
			f.values[key] = argv[i]
			generations[i] = argv[i]
		}
	}
	suffix := strings.Join(generations, ":")
	key := argv[n] + ":" + suffix
	sum := sha1.Sum(fmt.Appendf(nil, "%s:%s", argv[n+1], suffix))
	bucket := hex.EncodeToString(sum[:])
	entry := ""
	if argv[n+2] == "1" {
		entry = f.hashes[key]["sealed"]
	}
	reply := []any{key, bucket, entry}
	for _, generation := range generations {
		reply = append(reply, generation)
	}
	return reply, nil
}

func TestNearestMissesWhenPurgeFollowsTicketResolution(t *testing.T) {
	ring, err := secrets.ParseRingWithUnwrapper(context.Background(), fmt.Appendf(nil, `{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat("01", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeCommander{values: map[string]string{}, hashes: map[string]map[string]string{}}
	store := &Store{
		Client:       client,
		Keys:         ring,
		Auth:         secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), "installation"),
		Installation: "installation",
	}
	space := Space{Project: "project", Route: "route", NamedRoute: "named", Scope: "key", Fingerprint: "caller"}
	policy := cachepolicy.Policy{Mode: "semantic", EmbeddingRoute: "embeddings", AllowUnsealedVectors: true, TTLSeconds: 60}
	vector := []float32{1, 0}
	stored, err := store.Begin(t.Context(), space, []byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	stored.VectorSpace = "embed"
	if err := store.Put(t.Context(), stored, Entry{Body: []byte("answer")}, policy, cachepolicy.Controls{}, vector); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Begin(t.Context(), space, []byte("similar request"))
	if err != nil {
		t.Fatal(err)
	}
	ticket.VectorSpace = "embed"
	t.Run("before purge", func(t *testing.T) {
		entry, _, err := store.Nearest(t.Context(), ticket, vector, policy, cachepolicy.Controls{})
		if err != nil || entry == nil || string(entry.Body) != "answer" {
			t.Fatalf("got entry %v and %v before any purge, want the stored answer", entry, err)
		}
	})
	t.Run("failed search", func(t *testing.T) {
		client.searchErr = errors.New("search timed out")
		defer func() { client.searchErr = nil }()
		entry, _, err := store.Nearest(t.Context(), ticket, vector, policy, cachepolicy.Controls{})
		if entry != nil || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got entry %v and %v from partial search results, want cache unavailable", entry, err)
		}
	})
	bare := Ticket{Key: ticket.Key, Bucket: ticket.Bucket, VectorSpace: ticket.VectorSpace}
	if entry, _, err := store.Nearest(t.Context(), bare, vector, policy, cachepolicy.Controls{}); err != nil || entry != nil {
		t.Errorf("got entry %v and %v for a ticket without generation data, want a miss", entry, err)
	}
	if err := store.Purge(t.Context(), "project", space.Project); err != nil {
		t.Fatal(err)
	}
	entry, _, err := store.Nearest(t.Context(), ticket, vector, policy, cachepolicy.Controls{})
	if err != nil || entry != nil {
		t.Fatalf("got entry %v and %v after purging the project, want a miss", entry, err)
	}
}

func TestValidateVectorRefusesVectorsWithoutADirection(t *testing.T) {
	if err := ValidateVector([]float32{0, -0.5, 3}); err != nil {
		t.Error(err)
	}
	for name, vector := range map[string][]float32{
		"empty":     {},
		"zero":      {0, 0, 0},
		"NaN":       {1, float32(math.NaN())},
		"infinite":  {1, float32(math.Inf(-1))},
		"oversized": append(make([]float32, 65536), 1),
	} {
		if err := ValidateVector(vector); err == nil {
			t.Errorf("a %s vector was accepted", name)
		}
	}
}
