package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/coder/websocket"
)

// A peer can finish processing an already-read frame after cancellation. The
// coordinator must still join that application task before returning its facts.
type closingRelayPeer struct {
	frames chan []byte
	write  func(context.Context) error
}

func (p *closingRelayPeer) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case frame := <-p.frames:
		return websocket.MessageText, frame, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (p *closingRelayPeer) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	if p.write != nil {
		return p.write(ctx)
	}
	return nil
}

func (*closingRelayPeer) Ping(context.Context) error               { return nil }
func (*closingRelayPeer) Close(websocket.StatusCode, string) error { return nil }
func (*closingRelayPeer) CloseNow() error                          { return nil }

func TestDuplexCancellationJoinsProviderObservationBeforeReturning(t *testing.T) {
	for _, protocol := range []string{"realtime", "gemini_live", "native_bedrock"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered, complete := make(chan struct{}), make(chan struct{})
			defer close(complete)
			client := &closingRelayPeer{frames: make(chan []byte), write: func(ctx context.Context) error {
				close(entered)
				<-complete
				return ctx.Err()
			}}
			provider := &closingRelayPeer{frames: make(chan []byte, 1)}
			frame := `{"type":"response.done","response":{"id":"r","usage":{"input_tokens":9,"output_tokens":5}}}`
			if protocol == "gemini_live" {
				frame = `{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":9,"responseTokenCount":5}}`
			}
			var native *duplexcontract.Template
			if protocol == "native_bedrock" {
				h := bedrockDuplexHarness(t)
				route := h.rt.release.Snapshot.Routes[routeSlug]
				native, _ = h.rt.release.Snapshot.DuplexTemplate(routeSlug, route.Targets[0].ID)
				frame = `{"event":{"usageEvent":{"totalInputTokens":9,"totalOutputTokens":5,"totalTokens":14}}}`
			}
			provider.frames <- []byte(frame)
			x := &call{}
			s := &Server{}
			type result struct {
				usage *wire.Usage
				err   error
			}
			returned := make(chan result, 1)
			go func() {
				var u *wire.Usage
				var err error
				if protocol == "realtime" {
					u, _, err = s.relayRealtime(ctx, x, &pin{}, client, provider, "", "")
				} else if protocol == "gemini_live" {
					u, _, err = s.relayGeminiLive(ctx, x, &pin{}, client, provider, "", 1024)
				} else {
					u, _, err = s.relayNativeDuplex(ctx, x, &pin{}, client, provider, "", native.Observer(1024), native.Transport(), 1024)
				}
				returned <- result{u, err}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("provider terminal frame did not reach delivery")
			}
			cancel()
			select {
			case <-returned:
				t.Fatal("returned while provider frame processing was still owned by the relay")
			case <-time.After(20 * time.Millisecond):
			}
			complete <- struct{}{}
			select {
			case final := <-returned:
				if !errors.Is(final.err, context.Canceled) || final.usage == nil || final.usage.InputTokens != 9 || final.usage.OutputTokens != 5 {
					t.Fatalf("terminal observation lost on external shutdown: %+v, %v", final.usage, final.err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled relay did not join both directions")
			}
		})
	}
}
