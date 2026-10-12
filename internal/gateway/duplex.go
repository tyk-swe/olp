package gateway

import (
	"context"

	"github.com/coder/websocket"
)

// duplexPeer is the transport a session owns. Its two forwarding tasks must
// finish after cancellation and peer closure before their observations are read.
// Protocol validation and terminal obligations stay with each session owner.
type duplexPeer interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Ping(context.Context) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
}
