package workload

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Cache uses an identity-egress client supplied by the composition root. Fetches
// are serialized per issuer, refresh after a minute, and never use token URLs.
// A failed refresh may use verified cached keys for at most five minutes.
type Cache struct {
	client  *http.Client
	mu      sync.Mutex
	issuers map[string]*keyCache
}
type keyCache struct {
	mu                 sync.Mutex
	url                string
	keys               map[string]jose.JSONWebKey
	fetched, attempted time.Time
}

func NewCache(client *http.Client) *Cache {
	return &Cache{client: client, issuers: map[string]*keyCache{}}
}
func Fetch(ctx context.Context, client *http.Client, url string) (map[string]jose.JSONWebKey, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if e != nil {
		return nil, ErrKeys
	}
	req.Header.Set("Accept", "application/json")
	response, e := client.Do(req)
	if e != nil {
		return nil, ErrKeys
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrKeys
	}
	body, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if e != nil || len(body) > 1<<20 {
		return nil, ErrKeys
	}
	return ParseKeys(body)
}
func (c *Cache) Keys(ctx context.Context, id, url, kid string, now time.Time) (map[string]jose.JSONWebKey, error) {
	c.mu.Lock()
	entry := c.issuers[id]
	if entry == nil {
		entry = &keyCache{}
		c.issuers[id] = entry
	}
	c.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.url != url {
		entry.url = url
		entry.keys = nil
		entry.fetched = time.Time{}
		entry.attempted = time.Time{}
	}
	_, known := entry.keys[kid]
	if (entry.keys == nil || !known || now.Sub(entry.fetched) >= time.Minute) && now.Sub(entry.attempted) >= 15*time.Second {
		entry.attempted = now
		if keys, e := Fetch(ctx, c.client, url); e == nil {
			entry.keys = keys
			entry.fetched = now
		}
	}
	if entry.keys == nil || now.Sub(entry.fetched) >= 5*time.Minute {
		return nil, ErrKeys
	}
	return entry.keys, nil
}
