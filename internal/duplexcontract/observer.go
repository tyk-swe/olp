package duplexcontract

import (
	"sync"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type ServerFrame struct {
	ProviderError bool
	SetupComplete bool
}

type frameObserver interface {
	serverFrame(frame []byte) (ServerFrame, error)
	clientFrame(frame []byte) error
	usageValue() *wire.Usage
	complete() bool
}

type Observer struct {
	mu    sync.Mutex
	inner frameObserver
}

func (t *Template) Observer(maxBytes int) *Observer {
	switch t.profile.Dialect {
	case VertexRealtime:
		return &Observer{inner: &vertexObserver{maxBytes: maxBytes}}
	case BedrockRealtime:
		return &Observer{inner: &bedrockObserver{maxBytes: maxBytes}}
	}
	return &Observer{inner: &xaiObserver{maxBytes: maxBytes}}
}

func (o *Observer) ServerFrame(frame []byte) (ServerFrame, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.serverFrame(frame)
}

func (o *Observer) ClientFrame(frame []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.clientFrame(frame)
}

func (o *Observer) Usage() *wire.Usage {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.usageValue()
}

func (o *Observer) Complete() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.complete()
}
