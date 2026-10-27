package duplexcontract

import "github.com/tyk-swe/olp/internal/protocols/wire"

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
	inner frameObserver
}

func (t *Template) Observer(maxBytes int) *Observer {
	switch t.profile.Dialect {
	case VertexRealtime:
		return &Observer{inner: &vertexObserver{maxBytes: maxBytes}}
	}
	return &Observer{inner: &xaiObserver{maxBytes: maxBytes}}
}

func (o *Observer) ServerFrame(frame []byte) (ServerFrame, error) {
	return o.inner.serverFrame(frame)
}

func (o *Observer) ClientFrame(frame []byte) error { return o.inner.clientFrame(frame) }

func (o *Observer) Usage() *wire.Usage { return o.inner.usageValue() }

func (o *Observer) Complete() bool { return o.inner.complete() }
