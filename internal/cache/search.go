package cache

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/coordination"
)

// index returns the vector index for a dimension, creating it on first use.
// Creating with IF NOT EXISTS semantics is not available, so an existing index
// reports an error that FT.INFO confirms is benign.
func (s *Store) index(ctx context.Context, dimension int) (string, error) {
	name := s.prefix() + "vectors:" + strconv.Itoa(dimension)
	_, err := s.Client.Do(ctx, "FT.CREATE", name, "ON", "HASH", "PREFIX", "1", s.prefix()+"entry:", "SCHEMA", "bucket", "TAG", "vector", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", strconv.Itoa(dimension), "DISTANCE_METRIC", "COSINE")
	if err != nil {
		// Do not inspect or log server text: it may contain cache content.
		if _, check := s.Client.Do(ctx, "FT.INFO", name); check != nil {
			return "", ErrUnavailable
		}
	}
	return name, nil
}

func (s *Store) vectorBucket(ticket Ticket) string {
	if ticket.VectorSpace == "" {
		return ticket.Bucket
	}
	return s.digest([]string{ticket.Bucket, ticket.VectorSpace})
}

type candidate struct {
	key   string
	score float64
}

func (s *Store) generationsCurrent(ctx context.Context, ticket Ticket) (bool, error) {
	if len(ticket.GenerationKeys) == 0 || len(ticket.GenerationKeys) != len(ticket.GenerationValues) {
		return false, nil
	}
	args := append([]string{"MGET"}, ticket.GenerationKeys...)
	value, err := s.Client.Do(ctx, args...)
	if err != nil {
		return false, ErrUnavailable
	}
	list, ok := value.([]any)
	if !ok || len(list) != len(ticket.GenerationKeys) {
		return false, ErrUnavailable
	}
	for i, item := range list {
		current, err := replyString(item)
		if err != nil {
			return false, err
		}
		if current != ticket.GenerationValues[i] {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) Nearest(ctx context.Context, ticket Ticket, vector []float32, policy Policy, controls Controls) (*Entry, float64, error) {
	current, err := s.generationsCurrent(ctx, ticket)
	if err != nil {
		return nil, 0, err
	}
	if !current {
		return nil, 0, nil
	}
	p := policy.Effective()
	name, err := s.index(ctx, len(vector))
	if err != nil {
		return nil, 0, err
	}
	query := "(@bucket:{" + s.vectorBucket(ticket) + "})=>[KNN " + strconv.Itoa(p.MaxCandidates) + " @vector $v]"
	documents, err := s.Client.Search(ctx, name, query, "PARAMS", "2", "v", vectorBytes(vector), "RETURN", "1", "__vector_score", "LIMIT", "0", strconv.Itoa(p.MaxCandidates), "DIALECT", "2")
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	candidates, err := s.searchCandidates(documents, p)
	if err != nil {
		return nil, 0, err
	}
	for _, c := range candidates {
		entry, err := s.get(ctx, Ticket{Key: c.key, Bucket: ticket.Bucket, VectorSpace: ticket.VectorSpace}, controls)
		if err != nil {
			return nil, 0, err
		}
		if entry != nil {
			return entry, c.score, nil
		}
	}
	return nil, 0, nil
}

func (s *Store) searchCandidates(documents []coordination.SearchDocument, p Policy) ([]candidate, error) {
	candidates := make([]candidate, 0, len(documents))
	prefix := s.prefix() + "entry:"
	for _, document := range documents {
		if !strings.HasPrefix(document.Key, prefix) {
			return nil, ErrUnavailable
		}
		distance, err := strconv.ParseFloat(document.Doc["__vector_score"], 64)
		if err != nil {
			return nil, ErrUnavailable
		}
		similarity := 1 - distance
		if similarity >= p.SimilarityThreshold && similarity <= 1.000001 {
			candidates = append(candidates, candidate{document.Key, min(similarity, 1)})
		}
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return strings.Compare(a.key, b.key)
	})
	return candidates, nil
}
