package nativeduplex

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

type bedrockPeer struct {
	stream *connectors.BedrockStream
}

func (p *bedrockPeer) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	payload, err := p.stream.Receive(ctx)
	if errors.Is(err, connectors.ErrBedrockStreamClosed) {
		return 0, nil, websocket.CloseError{Code: websocket.StatusNormalClosure}
	}
	if err != nil {
		return 0, nil, err
	}
	return websocket.MessageText, payload, nil
}

func (p *bedrockPeer) Write(ctx context.Context, _ websocket.MessageType, payload []byte) error {
	return p.stream.Send(ctx, payload)
}

func (*bedrockPeer) Ping(context.Context) error { return nil }

func (p *bedrockPeer) Close(websocket.StatusCode, string) error { return p.stream.Close() }
func (p *bedrockPeer) CloseNow() error                          { return p.stream.Close() }

func (s surface) dialBedrockDuplex(ctx context.Context, x *kernel.Call, p *kernel.Pin) (kernel.DuplexPeer, *fault.Error) {
	endpoint, err := p.Provider.Connector().BedrockDuplexEndpoint()
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Bedrock duplex profile cannot address this model.")
	}
	return s.kernel.DialCustomDuplex(ctx, x, p, endpoint, func(dialCtx context.Context, auth *connectors.Auth, cfg connectors.Config, secret []byte, client *http.Client) (kernel.DuplexPeer, egress.Sensitive, error) {
		stream, applied, err := auth.OpenBedrockDuplex(dialCtx, cfg, secret, p.Model, client)
		if err != nil {
			return nil, applied, err
		}
		return &bedrockPeer{stream: stream}, applied, nil
	}, func(err error) (string, *fault.Error) {
		class := kernel.ClassConnect
		if errors.Is(err, connectors.ErrCredentialRejected) {
			class = kernel.ClassCredential
		}
		return class, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Bedrock bidirectional stream could not be opened.")
	})
}
