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
	// mu guards the fields below and is never held across a fetch; fetching
	// lets one caller at a time fetch.
	mu                 sync.Mutex
	fetching           sync.Mutex
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
	if entry.url != url {
		entry.url = url
		entry.keys = nil
		entry.fetched = time.Time{}
		entry.attempted = time.Time{}
	}
	keys, fetched, attempted := entry.keys, entry.fetched, entry.attempted
	entry.mu.Unlock()
	_, known := keys[kid]
	if known && now.Sub(fetched) < time.Minute {
		return keys, nil
	}
	if now.Sub(attempted) >= 15*time.Second {
		// A caller whose key is cached and usable never waits on a fetch, its
		// own or another's: it answers from the cache while at most one refresh
		// runs apart from any request, so a stalled issuer delays no token.
		if known && now.Sub(fetched) < 5*time.Minute {
			if entry.fetching.TryLock() {
				go func() {
					defer entry.fetching.Unlock()
					entry.refresh(context.WithoutCancel(ctx), c.client, url, now)
				}()
			}
			return keys, nil
		}
		entry.fetching.Lock()
		entry.refresh(ctx, c.client, url, now)
		entry.fetching.Unlock()
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.url != url || entry.keys == nil || now.Sub(entry.fetched) >= 5*time.Minute {
		return nil, ErrKeys
	}
	return entry.keys, nil
}

// refresh fetches the key set unless another caller attempted it while this
// one waited. The caller holds fetching.
func (e *keyCache) refresh(ctx context.Context, client *http.Client, url string, now time.Time) {
	e.mu.Lock()
	if e.url != url || now.Sub(e.attempted) < 15*time.Second {
		e.mu.Unlock()
		return
	}
	e.attempted = now
	e.mu.Unlock()
	keys, err := Fetch(ctx, client, url)
	if err != nil {
		return
	}
	e.mu.Lock()
	if e.url == url {
		e.keys = keys
		e.fetched = now
	}
	e.mu.Unlock()
}
