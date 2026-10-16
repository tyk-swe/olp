package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
)

var (
	ErrUnavailable = errors.New("response cache unavailable")
	ErrNotStored   = errors.New("response cache entry excluded by storage policy")
)

type Commander interface {
	Do(context.Context, ...string) (any, error)
}
type Store struct {
	Client       Commander
	Keys         *secrets.KeyRing
	Auth         *secrets.AuthKey
	Installation string
	Now          func() time.Time
}
type Space struct {
	Project     string `json:"project"`
	Route       string `json:"route"`
	Scope       string `json:"scope"`
	Fingerprint string `json:"fingerprint"`
}
type Ticket struct{ Key, Bucket string }
type Entry struct {
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Body      []byte          `json:"body,omitempty"`
	Frames    [][]byte        `json:"frames,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
}
type sealed struct {
	Version int    `json:"version"`
	Data    []byte `json:"data"`
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Store) prefix() string { return "olp:" + s.Installation + ":cache:" }
func (s *Store) digest(v any) string {
	b, _ := json.Marshal(v)
	return hex.EncodeToString(s.Auth.Digest(secrets.CacheKeyDigest, string(b)))
}
func (s *Store) generationKey(kind, id string) string {
	return s.prefix() + "generation:" + s.digest([]string{kind, id})
}

// The random generation must change when eviction removes its counter. A
// numeric counter recreated at zero could make old entries reachable again.
const lookupSource = `local g={}; for i,k in ipairs(KEYS) do local v=redis.call('GET',k); if not v then v=ARGV[i]; redis.call('SET',k,v) end; g[i]=v end; local suffix=g[1]..':'..g[2]; local key=ARGV[3]..':'..suffix; local bucket=redis.sha1hex(ARGV[4]..':'..suffix); local value=''; if ARGV[5]=='1' then value=redis.call('HGET',key,'sealed') or '' end; return {key,bucket,value}`

func (s *Store) resolve(ctx context.Context, space Space, canonical []byte, read bool) (Ticket, any, error) {
	if s == nil || s.Client == nil || s.Auth == nil || s.Keys == nil {
		return Ticket{}, nil, ErrUnavailable
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Ticket{}, nil, ErrUnavailable
	}
	flag := "0"
	if read {
		flag = "1"
	}
	base := s.prefix() + "entry:" + s.digest([]any{space, string(canonical)})
	result, err := s.Client.Do(ctx, "EVAL", lookupSource, "2", s.generationKey("project", space.Project), s.generationKey("route", space.Route), hex.EncodeToString(nonce[:16]), hex.EncodeToString(nonce[16:]), base, s.digest(space), flag)
	if err != nil {
		return Ticket{}, nil, ErrUnavailable
	}
	list, ok := result.([]any)
	if !ok || len(list) != 3 {
		return Ticket{}, nil, ErrUnavailable
	}
	key, kok := list[0].(string)
	bucket, bok := list[1].(string)
	if !kok || !bok {
		return Ticket{}, nil, ErrUnavailable
	}
	return Ticket{Key: key, Bucket: bucket}, list[2], nil
}
func (s *Store) Begin(ctx context.Context, space Space, canonical []byte) (Ticket, error) {
	ticket, _, err := s.resolve(ctx, space, canonical, false)
	return ticket, err
}

// Lookup resolves the current namespace and reads its entry atomically in one
// round trip. Purge and eviction cannot interleave those two decisions.
func (s *Store) Lookup(ctx context.Context, space Space, canonical []byte, controls Controls) (Ticket, *Entry, error) {
	ticket, value, err := s.resolve(ctx, space, canonical, true)
	if err != nil {
		return Ticket{}, nil, err
	}
	entry, err := s.open(ticket, value, controls)
	return ticket, entry, err
}
func (s *Store) Get(ctx context.Context, ticket Ticket, controls Controls) (*Entry, error) {
	value, err := s.Client.Do(ctx, "HGET", ticket.Key, "sealed")
	if err != nil {
		return nil, ErrUnavailable
	}
	return s.open(ticket, value, controls)
}
func (s *Store) open(ticket Ticket, value any, controls Controls) (*Entry, error) {
	if value == nil || value == "" {
		return nil, nil
	}
	raw, ok := value.(string)
	if !ok || len(raw) > 24<<20 {
		return nil, ErrUnavailable
	}
	var blob sealed
	if json.Unmarshal([]byte(raw), &blob) != nil || len(blob.Data) > 24<<20 {
		return nil, ErrUnavailable
	}
	plain, err := s.Keys.Open(s.Installation, secrets.ResponseCache, ticket.Key, blob.Version, blob.Data)
	if err != nil {
		return nil, ErrUnavailable
	}
	var entry Entry
	if json.Unmarshal(plain, &entry) != nil {
		return nil, ErrUnavailable
	}
	now := s.now()
	if entry.CreatedAt.After(now) || !entry.ExpiresAt.After(now) || controls.MaxAge != nil && now.Sub(entry.CreatedAt) > time.Duration(*controls.MaxAge)*time.Second {
		return nil, nil
	}
	return &entry, nil
}
func (s *Store) Put(ctx context.Context, ticket Ticket, entry Entry, policy Policy, controls Controls, vector []float32) error {
	if controls.NoStore {
		return ErrNotStored
	}
	p := policy.Effective()
	ttl := p.TTLSeconds
	if controls.TTL != nil {
		ttl = min(ttl, *controls.TTL)
	}
	if ttl == 0 {
		return ErrNotStored
	}
	entry.CreatedAt = s.now()
	entry.ExpiresAt = entry.CreatedAt.Add(time.Duration(ttl) * time.Second)
	plain, err := json.Marshal(entry)
	if err != nil || len(plain) > p.MaxEntryBytes {
		return ErrNotStored
	}
	ciphertext, err := s.Keys.Seal(s.Installation, secrets.ResponseCache, ticket.Key, plain)
	if err != nil {
		return ErrUnavailable
	}
	blob, _ := json.Marshal(sealed{Version: s.Keys.Active, Data: ciphertext})
	args := []string{"EVAL", `redis.call('HSET',KEYS[1],'sealed',ARGV[1],'bucket',ARGV[3]); if ARGV[4]~='' then redis.call('HSET',KEYS[1],'vector',ARGV[4]) end; redis.call('EXPIRE',KEYS[1],ARGV[2]); return 1`, "1", ticket.Key, string(blob), strconv.Itoa(ttl), ticket.Bucket, ""}
	if len(vector) > 0 {
		if _, err = s.index(ctx, len(vector)); err != nil {
			return err
		}
		args[len(args)-1] = vectorBytes(vector)
	}
	if _, err = s.Client.Do(ctx, args...); err != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Store) Purge(ctx context.Context, kind, id string) error {
	if s == nil || s.Client == nil || s.Auth == nil {
		return ErrUnavailable
	}
	if kind != "project" && kind != "route" {
		return errors.New("invalid purge scope")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrUnavailable
	}
	_, err := s.Client.Do(ctx, "SET", s.generationKey(kind, id), hex.EncodeToString(nonce[:]))
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

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
func (s *Store) Nearest(ctx context.Context, ticket Ticket, vector []float32, policy Policy, controls Controls) (*Entry, float64, error) {
	p := policy.Effective()
	name, err := s.index(ctx, len(vector))
	if err != nil {
		return nil, 0, err
	}
	query := "(@bucket:{" + ticket.Bucket + "})=>[KNN " + strconv.Itoa(p.MaxCandidates) + " @vector $v]"
	value, err := s.Client.Do(ctx, "FT.SEARCH", name, query, "PARAMS", "2", "v", vectorBytes(vector), "RETURN", "1", "__vector_score", "LIMIT", "0", strconv.Itoa(p.MaxCandidates), "DIALECT", "2")
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	list, ok := value.([]any)
	if !ok || len(list) < 1 {
		return nil, 0, ErrUnavailable
	}

	type candidate struct {
		key   string
		score float64
	}
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
	for i := 1; i < len(list); i++ {
		if results, ok := list[i].(map[string]any); ok {
			for key, value := range results {
				if err := appendCandidate(key, value); err != nil {
					return nil, 0, err
				}
			}
			continue
		}
		key, ok := list[i].(string)
		if !ok || i+1 >= len(list) {
			return nil, 0, ErrUnavailable
		}
		i++
		if err := appendCandidate(key, list[i]); err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].score > candidates[j].score
	})
	for _, c := range candidates {
		entry, err := s.Get(ctx, Ticket{Key: c.key, Bucket: ticket.Bucket}, controls)
		if err != nil {
			return nil, 0, err
		}
		if entry != nil {
			return entry, c.score, nil
		}
	}

	return nil, 0, nil
}
