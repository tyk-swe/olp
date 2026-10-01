package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// maxHosted bounds how many plugins' code a Host keeps.
const maxHosted = 16

// loadTimeout bounds reading and preparing one plugin's code.
const loadTimeout = 2 * time.Minute

// usableRecheck bounds how long the Host serves a plugin's cached code before
// observing again that the plugin remains usable, so its uninstall on another
// replica stops the code here too.
const usableRecheck = 30 * time.Second

// Host runs the code of installed plugins, by digest, for the processes that
// serve providers: a plugin profile's signing hook runs once per upstream
// request, and an unconfined plugin may carry the request itself (Carry); a
// grant's refresh runs in a worker ahead of its access token's expiry. A
// confined plugin's module is loaded from the database when it is prepared or
// on its first call, and kept compiled, with its pool of instances, for the
// calls after it, so no request compiles anything; an unconfined plugin's
// process likewise keeps running between calls. A Host keeps the code of the
// plugins it called most recently, and runs only plugins that Usable admits.
// It is safe for concurrent use.
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
	// verified is when the plugin's usability was last observed; checking
	// marks a recheck of it in flight.
	verified time.Time
	checking bool
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

// Manifest returns the manifest of the plugin with digest, which Usable must
// admit, such as for granting a call HTTP to the plugin's approved origins.
func (h *Host) Manifest(ctx context.Context, digest string) (abi.Manifest, error) {
	installed, err := usable(ctx, h.db, h.unconfined, digest, false)
	return installed.Manifest, err
}

// RefreshGrant runs the grant refresh of the plugin with digest for a grant
// of provider, granting it HTTP to the plugin's approved origins through
// client, the provider's network path, and redacting secrets from what the
// plugin logs and reports. A failure wraps connectors.ErrNotSent only when
// OLP knows the refresh could not have spent its token. A confined plugin can
// reach the upstream only through its HTTP capability; an unconfined plugin
// can reach it itself, so absence of HTTP activity proves nothing there.
func (h *Host) RefreshGrant(ctx context.Context, digest string, provider abi.Provider, refresh abi.GrantRefresh, client *http.Client, secrets []string) (abi.Grant, error) {
	var grant abi.Grant
	installed, err := usable(ctx, h.db, h.unconfined, digest, false)
	if err != nil {
		return grant, notSent(err)
	}
	var connected atomic.Bool
	// Once a connection is available, even a failed write may have sent part
	// of the request. Keep this evidence across every request of the refresh,
	// including a failed follow-up after the token endpoint succeeded.
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }})
	err = h.Call(ctx, digest, Call{Method: abi.MethodGrantRefresh, Params: refresh, Provider: &provider, Secrets: secrets, HTTP: &HTTP{Origins: installed.Manifest.Origins, Client: client}}, &grant)
	refusedBeforeStart := isCode(err, CodeExecutableChanged) || isCode(err, CodeExecutableInvalid)
	if err != nil && (installed.Executable == "" && !connected.Load() || refusedBeforeStart) {
		err = notSent(err)
	}
	return grant, err
}

// Prepare loads the code of the plugin with digest in the background, unless
// the Host holds it already, so the plugin's first call finds its module
// compiled, with an instance ready.
func (h *Host) Prepare(digest string) { h.done(h.use(digest)) }

// PreparePinned prepares the confined plugins that providers' drafts or active
// revisions pin, one at a time and the most recently approved first, as many
// as the Host keeps. A process calls it as it starts, so no request waits for
// a module to compile.
func (h *Host) PreparePinned(ctx context.Context) error {
	rows, err := h.db.Query(ctx, `SELECT pl.digest FROM olp.plugins pl
		WHERE pl.module IS NOT NULL AND pl.approved_at IS NOT NULL AND EXISTS (
			SELECT 1 FROM olp.providers p LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
			WHERE p.kind='plugin' AND p.configuration->>'profile_revision'=pl.digest
			   OR r.configuration->>'kind'='plugin' AND r.configuration->>'profile_revision'=pl.digest)
		ORDER BY pl.approved_at DESC LIMIT $1`, maxHosted)
	if err != nil {
		return err
	}
	digests, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, digest := range digests {
		entry := h.use(digest)
		_, err := entry.wait(ctx)
		h.done(entry)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			h.runtime.log.Warn("plugin could not be prepared", "plugin_digest", digest, "error", err)
		}
	}
	return nil
}

// Call serves call, of any ABI method, on the code of the plugin with digest
// and decodes its result into result. The callers of hosted code, such as a
// gateway attempt, record only that it failed, so the Host logs why.
func (h *Host) Call(ctx context.Context, digest string, call Call, result any) error {
	call.out = call.output(h.runtime.log, digest)
	entry := h.use(digest)
	defer h.done(entry)
	loaded, err := entry.wait(ctx)
	if err == nil {
		err = loaded.Call(ctx, call, result)
	}
	h.failed(ctx, call.out, call.Method, err)
	return err
}

// failed logs why a call of method failed within its output budget, unless
// its caller stopped waiting for it, or it is a device authorization's poll
// finding the device not approved yet, which its caller expects every
// interval.
func (h *Host) failed(ctx context.Context, out *output, method string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	if reported, ok := errors.AsType[*abi.Error](err); ok && method == abi.MethodGrantPoll &&
		(reported.Code == abi.CodeAuthorizationPending || reported.Code == abi.CodeSlowDown) {
		return
	}
	out.record(abi.LogRecord{Level: "warn", Message: "plugin call failed", Attrs: map[string]string{"error": err.Error()}})
}

// Evict drops the code the Host keeps for the plugin with digest, which its
// uninstall calls once committed, so this replica's cached copy stops serving
// and an unconfined plugin's process stops running. Other replicas' caches
// observe the removal when the usability of their copy is next rechecked.
func (h *Host) Evict(digest string) {
	h.mu.Lock()
	entry := h.hosted[digest]
	delete(h.hosted, digest)
	h.mu.Unlock()
	if entry == nil {
		return
	}
	entry.closeLoaded(context.Background())
}

// Close releases the code of every plugin the Host holds, stopping unconfined
// plugins' processes.
func (h *Host) Close(ctx context.Context) {
	h.mu.Lock()
	entries := h.hosted
	h.hosted = map[string]*hosted{}
	h.mu.Unlock()
	for _, entry := range entries {
		entry.closeLoaded(ctx)
	}
}

// wait returns the plugin's code once it is loaded.
func (entry *hosted) wait(ctx context.Context) (code, error) {
	select {
	case <-entry.loaded:
		return entry.code, entry.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// closeLoaded closes the entry's code once it is loaded. An entry still loading
// has left the Host's map, so its load closes the code it produced.
func (entry *hosted) closeLoaded(ctx context.Context) {
	select {
	case <-entry.loaded:
		if entry.code != nil {
			entry.code.Close(ctx)
		}
	default:
	}
}

// use returns the plugin's code for a call, starting to load it when the
// Host holds none, and rechecking the usability of a code held long enough
// that the plugin may have been uninstalled on another replica. The call's
// done ends its use.
func (h *Host) use(digest string) *hosted {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.hosted[digest]
	if entry == nil {
		entry = &hosted{loaded: make(chan struct{})}
		h.hosted[digest] = entry
		go h.load(digest, entry)
	} else if entry.code != nil && !entry.checking && time.Since(entry.verified) >= usableRecheck {
		entry.checking = true
		go h.recheck(digest, entry)
	}
	h.clock++
	entry.calls, entry.used = entry.calls+1, h.clock
	return entry
}

// done ends a call's use of a plugin's code.
func (h *Host) done(entry *hosted) {
	h.mu.Lock()
	entry.calls--
	var evicted []code
	if entry.calls == 0 {
		evicted = h.evictExcess()
	}
	h.mu.Unlock()
	for _, c := range evicted {
		c.Close(context.Background())
	}
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
	switch {
	case err != nil:
		if h.hosted[digest] == entry {
			delete(h.hosted, digest)
		}
	case h.hosted[digest] != entry:
		// Evicted while loading: the code it produced serves nobody.
		entry.err = refuse(CodeNotInstalled, "The plugin is no longer installed.")
		evicted = append(evicted, loaded)
	default:
		entry.code = loaded
		entry.verified = time.Now()
		evicted = h.evictExcess()
	}
	// Publish completion before eviction can take ownership of the code.
	close(entry.loaded)
	h.mu.Unlock()
	for _, c := range evicted {
		c.Close(ctx)
	}
}

// recheck observes whether the plugin a cached entry serves remains usable,
// evicting it and closing its code when a refusal — such as the plugin's
// uninstall on another replica — says it does not. Any other failure is no
// removal, and the entry stays until the check next comes due. It reads only
// the plugin's admission, never its module.
func (h *Host) recheck(digest string, entry *hosted) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	_, err := usable(ctx, h.db, h.unconfined, digest, false)
	h.mu.Lock()
	entry.checking = false
	_, refused := errors.AsType[*Error](err)
	stopped := false
	switch {
	case !refused:
		entry.verified = time.Now()
	case h.hosted[digest] == entry:
		delete(h.hosted, digest)
		stopped = true
	}
	h.mu.Unlock()
	if stopped {
		h.runtime.log.Warn("cached plugin stopped: it is no longer usable", "plugin_digest", digest, "error", err)
		entry.code.Close(ctx)
	}
}

// evictExcess forgets the least recently used code no call is using while the
// Host holds more than maxHosted plugins, and returns it to close.
func (h *Host) evictExcess() []code {
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
