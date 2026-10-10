package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secretstore"
)

const externalCredentialTTL = time.Minute
const maxExternalCredentials = 128

// ExternalSecrets is transport-only; all serving eligibility, pinned identity
// and cache decisions remain in Manager, the runtime credential source.
type ExternalSecrets interface {
	Resolve(context.Context, secretstore.Reference) ([]byte, error)
}

type externalCredential struct {
	reference         secretstore.Reference
	value             []byte
	expires, lastUsed time.Time
	loading           chan struct{}
}
type externalCredentials struct {
	mu        sync.Mutex
	entries   map[string]*externalCredential
	populated atomic.Bool
}

func (c *externalCredentials) available(id string) (known, available bool) {
	// Installations without external references need neither the cache lock nor
	// another clock read on every credential eligibility check.
	if !c.populated.Load() {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, known := c.entries[id]
	return known, known && len(entry.value) > 0 && time.Now().Before(entry.expires)
}

func (c *externalCredentials) read(ctx context.Context, resolver ExternalSecrets, id string, reference secretstore.Reference) ([]byte, error) {
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]*externalCredential{}
		}
		entry := c.entries[id]
		if entry != nil {
			if entry.reference != reference {
				c.mu.Unlock()
				return nil, ErrCredentialUnavailable
			}
			entry.lastUsed = time.Now()
			if len(entry.value) > 0 && time.Now().Before(entry.expires) {
				value := append([]byte(nil), entry.value...)
				c.mu.Unlock()
				return value, nil
			}
			if len(entry.value) == 0 && time.Now().Before(entry.expires) {
				c.mu.Unlock()
				return nil, ErrCredentialUnavailable
			}
			if entry.loading != nil {
				wait := entry.loading
				c.mu.Unlock()
				select {
				case <-wait:
					continue
				case <-ctx.Done():
					return nil, ErrCredentialUnavailable
				}
			}
			clear(entry.value)
			entry.value = nil
		} else {
			if len(c.entries) >= maxExternalCredentials {
				oldestID := ""
				var oldest time.Time
				for key, value := range c.entries {
					if value.loading == nil && (oldestID == "" || value.lastUsed.Before(oldest)) {
						oldestID, oldest = key, value.lastUsed
					}
				}
				if oldestID == "" {
					c.mu.Unlock()
					return nil, ErrCredentialUnavailable
				}
				clear(c.entries[oldestID].value)
				delete(c.entries, oldestID)
			}
			entry = &externalCredential{reference: reference, lastUsed: time.Now()}
			c.entries[id] = entry
			c.populated.Store(true)
		}
		entry.loading = make(chan struct{})
		c.mu.Unlock()
		var value []byte
		var err error
		if resolver == nil {
			err = ErrCredentialUnavailable
		} else {
			value, err = resolver.Resolve(ctx, reference)
		}
		if len(value) == 0 || len(value) > 64<<10 {
			err = ErrCredentialUnavailable
		}
		c.mu.Lock()
		if err == nil {
			entry.value = append([]byte(nil), value...)
			entry.expires = time.Now().Add(externalCredentialTTL)
		} else {
			entry.expires = time.Now().Add(PollInterval)
		}
		close(entry.loading)
		entry.loading = nil
		c.mu.Unlock()
		if err != nil {
			clear(value)
			return nil, ErrCredentialUnavailable
		}
		return value, nil
	}
}

func readExternalReferences(ctx context.Context, q access.Queryer, ids []string) (map[string]secretstore.Reference, error) {
	rows, err := q.Query(ctx, "SELECT id::text,external_reference FROM olp.provider_credentials WHERE id=ANY($1::uuid[]) AND external_reference IS NOT NULL", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]secretstore.Reference{}
	for rows.Next() {
		var id string
		var data []byte
		if err = rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var reference secretstore.Reference
		if json.Unmarshal(data, &reference) != nil || reference.Validate() != nil {
			return nil, errors.New("invalid external credential metadata")
		}
		result[id] = reference
	}
	return result, rows.Err()
}

func (m *Manager) refreshExternalCredentials(ctx context.Context) error {
	release := m.Release()
	references := map[string]secretstore.Reference{}
	m.external.mu.Lock()
	for id, entry := range m.external.entries {
		references[id] = entry.reference
	}
	m.external.mu.Unlock()
	for id, reference := range release.references {
		references[id] = reference
	}
	var failures []error
	for id, reference := range references {
		if _, err := m.external.read(ctx, m.ExternalSecrets, id, reference); err != nil {
			failures = append(failures, ErrCredentialUnavailable)
		}
	}
	return errors.Join(failures...)
}
