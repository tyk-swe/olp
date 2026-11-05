package upstream

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	return finishTransport(ctx, &Transport{wire: &streamTransport{connection: connection, gate: make(chan struct{}, 1), process: process}}, opts)
}
