package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
)

var (
	ErrUnavailable = errors.New("response cache unavailable")
	ErrNotStored   = errors.New("response cache entry excluded by storage policy")
)

// maxSealedBytes bounds a sealed entry read back from the store. It sits above
// the policy ceiling so malformed values fail before decryption.
const maxSealedBytes = 24 << 20

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
	NamedRoute  string `json:"named_route,omitempty"`
	Scope       string `json:"scope"`
	Fingerprint string `json:"fingerprint"`
}

type Ticket struct {
	Key, Bucket string
	// VectorSpace identifies the embedding model for semantic matches. Exact
	// lookups leave it empty so they do not need another embedding request.
	VectorSpace string
}

type Entry struct {
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Body      []byte          `json:"body,omitempty"`
	Frames    [][]byte        `json:"frames,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
}

type storedEntry struct {
	Bucket      string `json:"bucket"`
	VectorSpace string `json:"vector_space,omitempty"`
	Entry
}

type sealedBlob struct {
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

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// The random generation must change when eviction removes its counter. A
// numeric counter recreated at zero could make old entries reachable again.
const lookupScript = `
local generations = {}
for i, key in ipairs(KEYS) do
	local value = redis.call('GET', key)
	if not value then
		value = ARGV[i]
		redis.call('SET', key, value)
	end
	generations[i] = value
end
local suffix = table.concat(generations, ':')
local key = ARGV[#KEYS + 1] .. ':' .. suffix
local bucket = redis.sha1hex(ARGV[#KEYS + 2] .. ':' .. suffix)
local entry = ''
if ARGV[#KEYS + 3] == '1' then
	entry = redis.call('HGET', key, 'sealed') or ''
end
return {key, bucket, entry}
`

const putScript = `
redis.call('HSET', KEYS[1], 'sealed', ARGV[1], 'bucket', ARGV[3])
if ARGV[4] ~= '' then
	redis.call('HSET', KEYS[1], 'vector', ARGV[4])
end
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1
`

func (s *Store) resolve(ctx context.Context, space Space, canonical []byte, read bool) (Ticket, string, error) {
	if s == nil || s.Client == nil || s.Auth == nil || s.Keys == nil {
		return Ticket{}, "", ErrUnavailable
	}
	readFlag := "0"
	if read {
		readFlag = "1"
	}
	base := s.prefix() + "entry:" + s.digest([]any{space, string(canonical)})
	// Resolve both route generations atomically so purging either the named
	// route or its delegate invalidates exact entries and semantic candidates.
	generations := []string{s.generationKey("project", space.Project), s.generationKey("route", space.Route)}
	if space.NamedRoute != "" && space.NamedRoute != space.Route {
		generations = append(generations, s.generationKey("route", space.NamedRoute))
	}
	args := append([]string{"EVAL", lookupScript, strconv.Itoa(len(generations))}, generations...)
	for range generations {
		nonce, err := randomHex(16)
		if err != nil {
			return Ticket{}, "", ErrUnavailable
		}
		args = append(args, nonce)
	}
	args = append(args, base, s.digest(space), readFlag)
	result, err := s.Client.Do(ctx, args...)
	if err != nil {
		return Ticket{}, "", ErrUnavailable
	}
	list, ok := result.([]any)
	if !ok || len(list) != 3 {
		return Ticket{}, "", ErrUnavailable
	}
	key, kok := list[0].(string)
	bucket, bok := list[1].(string)
	if !kok || !bok {
		return Ticket{}, "", ErrUnavailable
	}
	entry, err := replyString(list[2])
	if err != nil {
		return Ticket{}, "", ErrUnavailable
	}
	return Ticket{Key: key, Bucket: bucket}, entry, nil
}

func (s *Store) Begin(ctx context.Context, space Space, canonical []byte) (Ticket, error) {
	ticket, _, err := s.resolve(ctx, space, canonical, false)
	return ticket, err
}

// Lookup resolves the current namespace and reads its entry atomically in one
// round trip. Purge and eviction cannot interleave those two decisions.
func (s *Store) Lookup(ctx context.Context, space Space, canonical []byte, controls Controls) (Ticket, *Entry, error) {
	ticket, raw, err := s.resolve(ctx, space, canonical, true)
	if err != nil {
		return Ticket{}, nil, err
	}
	entry, err := s.open(ticket, raw, controls)
	return ticket, entry, err
}

func (s *Store) get(ctx context.Context, ticket Ticket, controls Controls) (*Entry, error) {
	value, err := s.Client.Do(ctx, "HGET", ticket.Key, "sealed")
	if err != nil {
		return nil, ErrUnavailable
	}
	raw, err := replyString(value)
	if err != nil {
		return nil, err
	}
	return s.open(ticket, raw, controls)
}

// replyString reads a bulk-string reply: a missing value is a miss, while a
// non-string value breaks the client contract.
func replyString(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	raw, ok := value.(string)
	if !ok {
		return "", ErrUnavailable
	}
	return raw, nil
}

func (s *Store) open(ticket Ticket, raw string, controls Controls) (*Entry, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxSealedBytes {
		return nil, ErrUnavailable
	}
	var blob sealedBlob
	if json.Unmarshal([]byte(raw), &blob) != nil || len(blob.Data) > maxSealedBytes {
		return nil, ErrUnavailable
	}
	plain, err := s.Keys.Open(s.Installation, secrets.ResponseCache, ticket.Key, blob.Version, blob.Data)
	if err != nil {
		return nil, ErrUnavailable
	}
	var stored storedEntry
	if err := json.Unmarshal(plain, &stored); err != nil ||
		stored.Bucket == "" || stored.Bucket != ticket.Bucket ||
		ticket.VectorSpace != "" && stored.VectorSpace != ticket.VectorSpace {
		return nil, ErrUnavailable
	}
	entry := stored.Entry
	if !entry.fresh(s.now(), controls) {
		return nil, nil
	}
	return &entry, nil
}

// fresh reports whether the entry is live at now and within the caller's
// max-age control, when given.
func (e *Entry) fresh(now time.Time, controls Controls) bool {
	if e.CreatedAt.After(now) || !e.ExpiresAt.After(now) {
		return false
	}
	return controls.MaxAge == nil || now.Sub(e.CreatedAt) <= time.Duration(*controls.MaxAge)*time.Second
}

func (s *Store) Put(ctx context.Context, ticket Ticket, entry Entry, policy Policy, controls Controls, vector []float32) error {
	if s == nil || s.Client == nil || s.Auth == nil || s.Keys == nil {
		return ErrUnavailable
	}
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
	// Search metadata is untrusted. Bind the original scope and namespace
	// generation to the encrypted entry before accepting semantic candidates.
	plain, err := json.Marshal(storedEntry{Bucket: ticket.Bucket, VectorSpace: ticket.VectorSpace, Entry: entry})
	if err != nil || len(plain) > p.MaxEntryBytes {
		return ErrNotStored
	}
	ciphertext, err := s.Keys.Seal(s.Installation, secrets.ResponseCache, ticket.Key, plain)
	if err != nil {
		return ErrUnavailable
	}
	blob, _ := json.Marshal(sealedBlob{Version: s.Keys.Active, Data: ciphertext})
	vectorArg := ""
	if len(vector) > 0 {
		if _, err = s.index(ctx, len(vector)); err != nil {
			return err
		}
		vectorArg = vectorBytes(vector)
	}
	args := []string{"EVAL", putScript, "1", ticket.Key, string(blob), strconv.Itoa(ttl), s.vectorBucket(ticket), vectorArg}
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
	nonce, err := randomHex(16)
	if err != nil {
		return ErrUnavailable
	}
	if _, err = s.Client.Do(ctx, "SET", s.generationKey(kind, id), nonce); err != nil {
		return ErrUnavailable
	}
	return nil
}
