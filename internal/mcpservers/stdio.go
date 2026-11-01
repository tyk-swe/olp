package mcpservers

import (
	"context"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/routing"
)

func (e *Executables) Start(ctx context.Context, binding *routing.MCPStdioBinding, timeout time.Duration) (*plugins.NativeProcess, error) {
	if e.available() != nil || !binding.Valid() {
		return nil, ErrTransport
	}
	permitted, err := e.permitted(ctx, binding.Executable, binding.Digest)
	if err != nil || !permitted {
		return nil, ErrTransport
	}
	return e.Tier.StartNative(ctx, binding.Executable, binding.Digest, timeout)
}

func dialStdio(ctx context.Context, opts TransportOptions) (*Transport, error) {
	if opts.Executables == nil {
		return nil, ErrTransport
	}
	process, err := opts.Executables.Start(ctx, opts.Stdio, opts.Timeout)
	if err != nil {
		return nil, ErrTransport
	}
	connection, err := (&mcpsdk.IOTransport{Reader: process.Stdout, Writer: process.Stdin, MaxLineLength: 1 << 20}).Connect(ctx)
	if err != nil {
		process.Close()
		return nil, ErrTransport
	}
	return finishTransport(ctx, &Transport{stream: &streamTransport{connection: connection, gate: make(chan struct{}, 1), process: process}}, opts)
}
