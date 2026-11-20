//go:build !wasip1

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestCarryClosesResponseBodyOnce(t *testing.T) {
	t.Run("after EOF", func(t *testing.T) {
		body := &countingBody{ReadCloser: io.NopCloser(strings.NewReader("response"))}
		if err := carryWithBody(t.Context(), body); err != nil {
			t.Fatal(err)
		}
		if got := body.closes.Load(); got != 1 {
			t.Errorf("response body closed %d times, want 1", got)
		}
	})

	t.Run("on cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			body := &countingBody{ReadCloser: reader}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- carryWithBody(ctx, body)
			}()
			synctest.Wait()
			cancel()
			synctest.Wait()
			select {
			case err := <-result:
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("carry returned %v, want a read interrupted by Close", err)
				}
			default:
				t.Fatal("cancellation did not unblock the response body read")
			}
			if got := body.closes.Load(); got != 1 {
				t.Errorf("response body closed %d times, want 1", got)
			}
		})
	})
}

func carryWithBody(ctx context.Context, body io.ReadCloser) error {
	previous := registered
	registered = testCarrier{body: body}
	defer func() { registered = previous }()
	previousSession := serving.Swap(&session{out: io.Discard})
	defer serving.Store(previousSession)
	_, err := carry(ctx, json.RawMessage(`{}`))
	return err
}

type testCarrier struct{ body io.ReadCloser }

func (testCarrier) Manifest() Manifest { return Manifest{} }

func (c testCarrier) Carry(context.Context, HTTPRequest) (CarriedResponse, error) {
	return CarriedResponse{Status: 200, Body: c.body}, nil
}

type countingBody struct {
	io.ReadCloser
	closes atomic.Int32
}

func (b *countingBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}
