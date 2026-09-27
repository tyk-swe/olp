package plugins

import (
	"context"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// maxHosted bounds how many plugins' code a Host keeps.
const maxHosted = 16

// loadTimeout bounds reading and preparing one plugin's code.
const loadTimeout = 2 * time.Minute

// Host runs the code of installed plugins, by digest, for the processes that
// serve providers: a plugin profile's signing hook runs once per upstream
// request. A confined plugin's module is loaded from the database on its
// first call and kept compiled, with its pool of instances, for the calls
// after it, so no request compiles anything; an unconfined plugin's process
// likewise keeps running between calls. A Host keeps the code of the plugins
// it called most recently, and runs only plugins that Usable admits. It is
// safe for concurrent use.
type Host struct {
	runtime    *Runtime
	unconfined *Unconfined
	db         access.Queryer
	mu         sync.Mutex
	hosted     map[string]*hosted
	clock      uint64
}

// code is a plugin's code, which a Host calls across the ABI: a confined
// plugin's *Module or an unconfined plugin's *Executable.
type code interface {
	Call(ctx context.Context, call Call, result any) error
	Close(ctx context.Context) error
}

// hosted is one plugin's code, loading or loaded.
type hosted struct {
	// loaded is closed once loading ends, with code or err set.
	loaded chan struct{}
	code   code
	err    error
	// calls counts the calls using the code, and used orders its uses.
	calls int
	used  uint64
}

// NewHost returns a Host that runs confined plugins' modules on runtime and,
// where unconfined is not nil, unconfined plugins' executables in that tier,
// reading which plugins are installed from db.
func NewHost(runtime *Runtime, unconfined *Unconfined, db access.Queryer) *Host {
	return &Host{runtime: runtime, unconfined: unconfined, db: db, hosted: map[string]*hosted{}}
}

// Sign runs the signing hook of the plugin with digest over request for
// provider, redacting secrets from what the plugin logs and reports.
func (h *Host) Sign(ctx context.Context, digest string, provider abi.Provider, request abi.SignRequest, secrets []string) (abi.SignResult, error) {
	var result abi.SignResult
	err := h.Call(ctx, digest, Call{Method: abi.MethodSign, Params: request, Provider: &provider, Secrets: secrets}, &result)
	return result, err
}

// Call serves call, of any ABI method, on the code of the plugin with digest
// and decodes its result into result. The callers of hosted code, such as a
// gateway attempt, record only that it failed, so the Host logs why.
func (h *Host) Call(ctx context.Context, digest string, call Call, result any) error {
	entry := h.use(digest)
	defer func() {
		h.mu.Lock()
		entry.calls--
		h.mu.Unlock()
	}()
	var err error
	select {
	case <-entry.loaded:
		err = entry.err
		if err == nil {
			err = entry.code.Call(ctx, call, result)
		}
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil && ctx.Err() == nil {
		h.runtime.log.Warn("plugin call failed", "plugin_digest", digest, "plugin_method", call.Method, "error", err)
	}
	return err
}

// Close releases the code of every plugin the Host holds, stopping unconfined
// plugins' processes.
func (h *Host) Close(ctx context.Context) {
	h.mu.Lock()
	entries := h.hosted
	h.hosted = map[string]*hosted{}
	h.mu.Unlock()
	for _, entry := range entries {
		select {
		case <-entry.loaded:
			if entry.code != nil {
				entry.code.Close(ctx)
			}
		default:
			// Still loading, so nothing runs it yet.
		}
	}
}

// use returns the plugin's code for a call, starting to load it when the
// Host holds none.
func (h *Host) use(digest string) *hosted {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.hosted[digest]
	if entry == nil {
		entry = &hosted{loaded: make(chan struct{})}
		h.hosted[digest] = entry
		go h.load(digest, entry)
	}
	h.clock++
	entry.calls, entry.used = entry.calls+1, h.clock
	return entry
}

// load reads a plugin and prepares its code: it compiles a confined plugin's
// module. It runs apart from the call that started it, so a caller that stops
// waiting wastes no compilation. A failed load is forgotten, so a later call
// tries again.
func (h *Host) load(digest string, entry *hosted) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	var loaded code
	installed, err := Usable(ctx, h.db, h.unconfined, digest)
	switch {
	case err != nil:
	case installed.Executable != "":
		loaded = h.unconfined.Load(digest, installed.Executable)
	default:
		loaded, err = h.runtime.Load(ctx, installed.Module)
	}
	h.mu.Lock()
	entry.err = err
	var evicted []code
	if err != nil {
		delete(h.hosted, digest)
	} else {
		entry.code = loaded
		evicted = h.evict()
	}
	h.mu.Unlock()
	close(entry.loaded)
	for _, c := range evicted {
		c.Close(ctx)
	}
}

// evict forgets the least recently used code no call is using while the Host
// holds more than maxHosted plugins, and returns it to close.
func (h *Host) evict() []code {
	var evicted []code
	for len(h.hosted) > maxHosted {
		var oldest *hosted
		var digest string
		for d, entry := range h.hosted {
			if entry.code != nil && entry.calls == 0 && (oldest == nil || entry.used < oldest.used) {
				oldest, digest = entry, d
			}
		}
		if oldest == nil {
			break
		}
		delete(h.hosted, digest)
		evicted = append(evicted, oldest.code)
	}
	return evicted
}
