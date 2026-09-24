package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestRealtimeBoundedWriterRenewsIndependentFrameDeadline(t *testing.T) {
	writer := newRealtimeBoundedWriter(t.Context(), realtimeWriteFixture{}, 20*time.Millisecond)
	defer writer.close()
	if err := writer.write(websocket.MessageText, []byte(`{"type":"first"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	if err := writer.write(websocket.MessageText, []byte(`{"type":"second"}`)); err != nil {
		t.Fatalf("idle gap consumed the next frame's deadline: %v", err)
	}
}

func TestRealtimeBoundedWriterStopsSlowPeer(t *testing.T) {
	for _, peer := range []string{"client", "provider"} {
		t.Run(peer, func(t *testing.T) {
			writer := newRealtimeBoundedWriter(t.Context(), realtimeWriteFixture{wait: true}, 20*time.Millisecond)
			defer writer.close()
			started := time.Now()
			if err := writer.write(websocket.MessageText, []byte(`{"type":"stalled"}`)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("slow %s peer was not terminated at its frame deadline: %v", peer, err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("slow %s peer held the writer for %v", peer, elapsed)
			}
		})
	}
}

func TestRealtimeBoundedWriterPropagatesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	writer := newRealtimeBoundedWriter(ctx, realtimeWriteFixture{wait: true}, time.Second)
	defer writer.close()
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	if err := writer.write(websocket.MessageText, []byte(`{"type":"cancelled"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation changed to a write timeout: %v", err)
	}
}
