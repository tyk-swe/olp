package plugins

import (
	"context"
	"errors"
	"sync"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// PrepareGuardrail is called during release installation. Serving never loads
// modules or reads management configuration as a side effect of a request.
func (h *Host) PrepareGuardrail(ctx context.Context, digest string) error {
	release, err := h.PinGuardrail(ctx, digest)
	if err == nil {
		release()
	}
	return err
}

// PinGuardrail keeps an approved module resident for a runtime release. The
// returned function releases its cache reference after no request uses that
// release, so ordinary provider traffic cannot evict a pinned guardrail.
func (h *Host) PinGuardrail(ctx context.Context, digest string) (func(), error) {
	manifest, err := h.Manifest(ctx, digest)
	if err != nil || !manifest.Guardrail {
		return nil, errors.New("approved guardrail plugin required")
	}
	entry := h.use(digest)
	loaded, err := entry.wait(ctx)
	if err != nil {
		h.done(entry)
		return nil, errors.New("guardrail plugin could not be loaded")
	}
	if _, ok := loaded.(*Module); !ok {
		h.done(entry)
		return nil, errors.New("guardrails require confined WASM")
	}
	var once sync.Once
	return func() { once.Do(func() { h.done(entry) }) }, nil
}

func (h *Host) Guardrail(ctx context.Context, digest string, input abi.GuardrailRequest) (abi.GuardrailVerdict, error) {
	h.mu.Lock()
	entry := h.hosted[digest]
	if entry == nil {
		h.mu.Unlock()
		return abi.GuardrailVerdict{}, errors.New("guardrail plugin is not preloaded")
	}
	entry.calls++
	h.mu.Unlock()
	defer h.done(entry)
	loaded, err := entry.wait(ctx)
	if err != nil {
		return abi.GuardrailVerdict{}, errors.New("guardrail plugin unavailable")
	}
	if _, ok := loaded.(*Module); !ok {
		return abi.GuardrailVerdict{}, errors.New("guardrails require confined WASM")
	}
	var verdict abi.GuardrailVerdict
	// No HTTP capability is granted: this method is confined to local compute.
	err = loaded.Call(ctx, Call{Method: abi.MethodGuardrail, Params: input, SuppressDiagnostics: true}, &verdict)
	if err != nil {
		return abi.GuardrailVerdict{}, errors.New("guardrail plugin evaluation failed")
	}
	return verdict, nil
}
