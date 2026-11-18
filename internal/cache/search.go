package cache

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

func textValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func fields(v any) map[string]string {
	out := map[string]string{}
	if list, ok := v.([]any); ok {
		for i := 0; i+1 < len(list); i += 2 {
			out[textValue(list[i])] = textValue(list[i+1])
		}
	}
	if m, ok := v.(map[string]any); ok {
		for k, v := range m {
			out[k] = textValue(v)
		}
	}
	return out
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
	value, err := s.Client.Do(ctx, "FT.SEARCH", name, query, "PARAMS", "2", "v", vectorBytes(vector), "RETURN", "1", "__vector_score", "LIMIT", "0", strconv.Itoa(p.MaxCandidates), "DIALECT", "2")
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	candidates, err := s.searchCandidates(value, p)
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

func (s *Store) searchCandidates(value any, p Policy) ([]candidate, error) {
	candidates := []candidate{}
	appendCandidate := func(key string, value any) error {
		if !strings.HasPrefix(key, s.prefix()+"entry:") {
			return ErrUnavailable
		}
		distance, err := strconv.ParseFloat(fields(value)["__vector_score"], 64)
		if err != nil {
			return ErrUnavailable
		}
		similarity := 1 - distance
		if similarity >= p.SimilarityThreshold && similarity <= 1.000001 {
			candidates = append(candidates, candidate{key, min(similarity, 1)})
		}
		return nil
	}
	if err := visitSearchResults(value, appendCandidate); err != nil {
		return nil, err
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return strings.Compare(a.key, b.key)
	})
	return candidates, nil
}

func visitSearchResults(value any, visit func(string, any) error) error {
	if response, ok := value.(map[string]any); ok {
		results, ok := response["results"].([]any)
		if !ok {
			return ErrUnavailable
		}
		for _, item := range results {
			result, _ := item.(map[string]any)
			key, ok := result["id"].(string)
			if !ok {
				return ErrUnavailable
			}
			if err := visit(key, result["extra_attributes"]); err != nil {
				return err
			}
		}
		return nil
	}
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return ErrUnavailable
	}
	for i := 1; i < len(list); i++ {
		if results, ok := list[i].(map[string]any); ok {
			for key, value := range results {
				if err := visit(key, value); err != nil {
					return err
				}
			}
			continue
		}
		key, ok := list[i].(string)
		if !ok || i+1 >= len(list) {
			return ErrUnavailable
		}
		i++
		if err := visit(key, list[i]); err != nil {
			return err
		}
	}
	return nil
}
