//go:build integration

package cache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/coordination"
)

func TestIntegrationCacheExactSemanticAndPurge(t *testing.T) {
	url := os.Getenv("OLP_TEST_CACHE_VALKEY_URL")
	if url == "" {
		t.Fatal("OLP_TEST_CACHE_VALKEY_URL is required")
	}
	cfg, err := coordination.Configuration(url, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := coordination.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s, _ := testStore(t)
	s.Client = client
	s.Installation = uuid.NewString()
	ctx := t.Context()
	space := Space{Project: "p", Route: "r", Scope: "k", Fingerprint: "v1"}
	ticket, err := s.Begin(ctx, space, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Mode: "semantic", TTLSeconds: 600, AllowUnsealedVectors: true, EmbeddingRoute: "embeddings"}
	e := Entry{Body: []byte("secret response"), Metadata: json.RawMessage(`{}`)}
	if err = s.Put(ctx, ticket, e, p, Controls{}, []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	raw, err := client.Do(ctx, "HGET", ticket.Key, "sealed")
	if err != nil || strings.Contains(textValue(raw), "secret response") {
		t.Fatal("response was not sealed", err)
	}
	query, _ := s.Begin(ctx, space, []byte("nearby"))
	deadline := time.Now().Add(5 * time.Second)
	var hit *Entry
	var similarity float64
	for time.Now().Before(deadline) {
		hit, similarity, err = s.Nearest(ctx, query, []float32{.999, .001, 0}, p, Controls{})
		if err != nil {
			t.Fatal(err)
		}
		if hit != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hit == nil || similarity < .99 || string(hit.Body) != "secret response" {
		t.Fatalf("semantic match failed: %v %.4f", hit, similarity)
	}
	otherSpace := space
	otherSpace.Scope = "another-key"
	other, err := s.Begin(ctx, otherSpace, []byte("nearby"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(ctx, "HSET", ticket.Key, "bucket", other.Bucket); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hit, _, err = s.Nearest(ctx, other, []float32{1, 0, 0}, p, Controls{})
		if hit != nil {
			t.Fatal("tampered search bucket exposed another key's response")
		}
		if errors.Is(err, ErrUnavailable) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("tampered search candidate was not authenticated")
	}
	if err = s.Purge(ctx, "project", "p"); err != nil {
		t.Fatal(err)
	}
	query, _ = s.Begin(ctx, space, []byte("nearby"))
	hit, _, err = s.Nearest(ctx, query, []float32{1, 0, 0}, p, Controls{})
	if err != nil || hit != nil {
		t.Fatal("purge retained semantic match", err)
	}
	if _, err = client.Do(ctx, "FT.DROPINDEX", s.prefix()+"vectors:3"); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Do(ctx, "DEL", ticket.Key, s.generationKey("route", "r"), s.generationKey("project", "p"))
}

func TestIntegrationCacheRejectsCoordinationInstance(t *testing.T) {
	url := os.Getenv("OLP_TEST_VALKEY_URL")
	if url == "" {
		t.Fatal("OLP_TEST_VALKEY_URL is required")
	}
	cfg, err := coordination.Configuration(url, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	main, err := coordination.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	remote, err := NewRemote(url, "", main)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = remote.Do(ctx, "PING"); err == nil {
		t.Fatal("cache accepted coordination server")
	}
}
