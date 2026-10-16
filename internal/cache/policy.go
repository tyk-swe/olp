// Package cache owns the opt-in, sealed response cache. It never stores content
// in the control database or in the coordination Valkey.
package cache

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type Policy struct {
	Mode                 string   `json:"mode"`
	TTLSeconds           int      `json:"ttl_seconds,omitempty"`
	Scope                string   `json:"scope,omitempty"`
	Operations           []string `json:"operations,omitempty"`
	Streaming            string   `json:"streaming,omitempty"`
	MaxEntryBytes        int      `json:"max_entry_bytes,omitempty"`
	CountHitsAgainst     []string `json:"count_hits_against,omitempty"`
	EmbeddingRoute       string   `json:"embedding_route,omitempty"`
	SimilarityThreshold  float64  `json:"similarity_threshold,omitempty"`
	MaxCandidates        int      `json:"max_candidates,omitempty"`
	AllowUnsealedVectors bool     `json:"allow_unsealed_vectors,omitempty"`
	AllowMultiturn       bool     `json:"allow_multiturn,omitempty"`
}

func (p Policy) Effective() Policy {
	if p.TTLSeconds == 0 {
		p.TTLSeconds = 3600
	}
	if p.Scope == "" {
		p.Scope = "key"
	}
	if p.Streaming == "" {
		p.Streaming = "replay"
	}
	if p.MaxEntryBytes == 0 {
		p.MaxEntryBytes = 1 << 20
	}
	if len(p.Operations) == 0 {
		p.Operations = []string{"generation", "embeddings", "rerank", "moderation"}
		if p.Mode == "semantic" {
			p.Operations = []string{"generation"}
		}
	}
	if len(p.CountHitsAgainst) == 0 {
		p.CountHitsAgainst = []string{"requests"}
	}
	if p.SimilarityThreshold == 0 {
		p.SimilarityThreshold = .95
	}
	if p.MaxCandidates == 0 {
		p.MaxCandidates = 5
	}
	return p
}

func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	e := p.Effective()
	if e.Mode != "exact" && e.Mode != "semantic" {
		return errors.New("cache.mode must be exact or semantic")
	}
	if !slices.Contains([]string{"key", "project", "route"}, e.Scope) {
		return errors.New("cache.scope must be key, project or route")
	}
	if e.TTLSeconds < 1 || e.TTLSeconds > 604800 || e.MaxEntryBytes < 1 || e.MaxEntryBytes > 16<<20 {
		return errors.New("cache TTL must be 1–604800 seconds and entries 1–16777216 bytes")
	}
	if e.Streaming != "replay" && e.Streaming != "bypass" {
		return errors.New("cache.streaming must be replay or bypass")
	}
	if !uniqueMembers(e.Operations, []string{"generation", "embeddings", "rerank", "moderation"}) {
		return errors.New("cache.operations contains unsupported or duplicate operations")
	}
	if !uniqueMembers(e.CountHitsAgainst, []string{"requests", "tokens", "budgets"}) || !slices.Contains(e.CountHitsAgainst, "requests") {
		return errors.New("cache.count_hits_against must contain requests and may include tokens and budgets")
	}
	if e.Mode == "semantic" {
		if !e.AllowUnsealedVectors || !validName(e.EmbeddingRoute) {
			return errors.New("semantic caching requires an embedding route and allow_unsealed_vectors acknowledgement")
		}
		if !(e.SimilarityThreshold > 0 && e.SimilarityThreshold <= 1) || e.MaxCandidates < 1 || e.MaxCandidates > 32 {
			return errors.New("semantic similarity must be greater than zero and at most one, with 1–32 candidates")
		}
		if len(e.Operations) != 1 || e.Operations[0] != "generation" {
			return errors.New("semantic caching supports only generation; set operations to [generation]")
		}
	} else if e.EmbeddingRoute != "" || e.AllowUnsealedVectors || e.AllowMultiturn {
		return errors.New("semantic options require cache.mode semantic")
	}
	return nil
}

func uniqueMembers(values, allowed []string) bool {
	for i, v := range values {
		if !slices.Contains(allowed, v) || slices.Contains(values[:i], v) {
			return false
		}
	}
	return len(values) > 0
}
func validName(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}

type PromptPolicy struct {
	Positions    []string `json:"positions,omitempty"`
	TTL          string   `json:"ttl,omitempty"`
	KeySource    string   `json:"key_source,omitempty"`
	SessionLabel string   `json:"session_label,omitempty"`
}

func (p *PromptPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if p.TTL != "" && p.TTL != "5m" && p.TTL != "1h" {
		return errors.New("prompt_cache.ttl must be 5m or 1h")
	}
	if len(p.Positions) > 0 && !uniqueMembers(p.Positions, []string{"system", "tools", "last_user"}) {
		return errors.New("prompt_cache.positions must contain unique system, tools or last_user positions")
	}
	if p.KeySource != "" && p.KeySource != "key" && p.KeySource != "session" {
		return errors.New("prompt_cache.key_source must be key or session")
	}
	if p.KeySource == "session" && (len(p.SessionLabel) == 0 || len(p.SessionLabel) > 32) {
		return errors.New("prompt_cache.session_label is required for session keys")
	}
	return nil
}

type Controls struct {
	NoCache, NoStore bool
	MaxAge, TTL      *int
}

func ParseControls(raw string) (Controls, error) {
	var c Controls
	if len(raw) > 512 {
		return c, errors.New("X-OLP-Cache is too long")
	}
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, value, hasValue := strings.Cut(item, "=")
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if seen[name] {
			return c, errors.New("duplicate X-OLP-Cache directive")
		}
		seen[name] = true
		switch name {
		case "no-cache", "no-store":
			if hasValue {
				return c, errors.New("cache flag cannot have a value")
			}
			if name == "no-cache" {
				c.NoCache = true
			} else {
				c.NoStore = true
			}
		case "max-age", "ttl":
			n, err := strconv.Atoi(value)
			if !hasValue || err != nil || n < 0 || n > 604800 {
				return c, errors.New("cache age and TTL must be 0–604800 seconds")
			}
			if name == "max-age" {
				c.MaxAge = &n
			} else {
				c.TTL = &n
			}
		default:
			return c, fmt.Errorf("unsupported X-OLP-Cache directive")
		}
	}
	return c, nil
}
