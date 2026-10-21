package gateway

import (
	"context"

	"github.com/coder/websocket"
)

type stalledGeminiWriter struct{}

func (stalledGeminiWriter) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	<-ctx.Done()
	return ctx.Err()
}
