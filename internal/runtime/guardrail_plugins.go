package runtime

import "sync"

// A release owns these cache references. Its GC cleanup can only run after
// the manager and all requests have stopped retaining the release.
type guardrailPluginPins struct {
	mu      sync.Mutex
	release []func()
}

func (p *guardrailPluginPins) add(release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.release = append(p.release, release)
}
func (p *guardrailPluginPins) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, release := range p.release {
		if release != nil {
			release()
		}
	}
	p.release = nil
}
