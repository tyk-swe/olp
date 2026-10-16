package gateway

import (
	"context"

	"github.com/coder/websocket"
)

type realtimeWriteFixture struct{ wait bool }

func (f realtimeWriteFixture) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	if !f.wait {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}
