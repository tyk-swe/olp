package egress

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ConnectionClientCache bounds the number of retained transport configurations.
// Each service owns one cache. Credential resolution and current revocation
// checks remain with the caller; a cached client is never credential authority.
// Cache keys retain digests only, not plaintext network credentials.
// Returned clients are shared and must not be mutated by callers.
type ConnectionClientCache struct {
	mu       sync.Mutex
	capacity int
	entries  map[[32]byte]*list.Element
	order    list.List
}

type connectionCacheEntry struct {
	key    [32]byte
	client *http.Client
}

// NewConnectionClientCache constructs a bounded cache. Capacity is a local
// service setting; invalid values are programmer errors, not provider input.
func NewConnectionClientCache(capacity int) *ConnectionClientCache {
	if capacity < 1 || capacity > 4096 {
		panic("connection client cache capacity must be between 1 and 4096")
	}
	return &ConnectionClientCache{capacity: capacity, entries: make(map[[32]byte]*list.Element)}
}

// Client reuses connections within a standalone, single-authority cache.
// Provider-serving call sites must use ClientScoped to isolate their providers,
// revisions and credential principals even when network options are identical.
func (c *ConnectionClientCache) Client(policy Policy, options *ConnectionOptions, secret []byte, defaultResponseHeaderTimeout time.Duration) (*http.Client, error) {
	return c.ClientScoped("standalone", policy, options, secret, defaultResponseHeaderTimeout)
}

// ClientScoped reuses a connection pool only within the caller's authority
// scope. The scope must include provider/tenant ownership, serving revision and
// credential principal identity as applicable; it must not contain secrets.
// Credential resolution and current revocation checks remain with the caller.
func (c *ConnectionClientCache) ClientScoped(scope string, policy Policy, options *ConnectionOptions, secret []byte, defaultResponseHeaderTimeout time.Duration) (*http.Client, error) {
	if scope == "" || len(scope) > 1024 || strings.TrimSpace(scope) != scope || strings.ContainsAny(scope, "\r\n\x00") {
		return nil, errors.New("connection client scope must be a nonempty opaque identity of at most 1024 bytes")
	}
	if c == nil {
		return nil, errors.New("connection client cache is unavailable")
	}
	keyDocument, err := json.Marshal(struct {
		Scope                 string
		Policy                Policy
		Options               *ConnectionOptions
		SecretDigest          [32]byte
		ResponseHeaderTimeout time.Duration
	}{scope, policy, options, sha256.Sum256(secret), defaultResponseHeaderTimeout})
	if err != nil {
		return nil, errors.New("invalid connection cache configuration")
	}
	key := sha256.Sum256(keyDocument)
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		c.order.MoveToFront(entry)
		return entry.Value.(connectionCacheEntry).client, nil
	}
	client, err := policy.ConnectionClient(options, secret, defaultResponseHeaderTimeout)
	if err != nil {
		return nil, err
	}
	entry := c.order.PushFront(connectionCacheEntry{key: key, client: client})
	c.entries[key] = entry
	if c.order.Len() > c.capacity {
		oldest := c.order.Back()
		retired := oldest.Value.(connectionCacheEntry)
		delete(c.entries, retired.key)
		c.order.Remove(oldest)
		retired.client.Transport.(*connectionTransport).retire()
	}
	return client, nil
}

// CloseIdleConnections closes all retained idle pools and forgets their cache
// entries. Existing in-flight requests retain their transport until completion.
func (c *ConnectionClientCache) CloseIdleConnections() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, element := range c.entries {
		element.Value.(connectionCacheEntry).client.Transport.(*connectionTransport).retire()
	}
	c.entries = make(map[[32]byte]*list.Element)
	c.order.Init()
}
