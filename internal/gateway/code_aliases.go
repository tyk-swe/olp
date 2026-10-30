package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

func (a *codeAttempt) copyModelResponse(ctx context.Context, w http.ResponseWriter, response *http.Response) error {
	reader, err := codewire.OpenDecoder(response.Body, response.Header.Get("Content-Encoding"), codewire.MaxBody)
	if err != nil {
		return codemode.Refuse(502, "code_upstream_invalid")
	}
	defer reader.Close()
	rc := http.NewResponseController(w)
	deliver := func(body []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		if _, err := w.Write(body); err != nil {
			a.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
			return false
		}
		if err := rc.Flush(); err != nil {
			a.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
			return false
		}
		return true
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		body, readErr := io.ReadAll(io.LimitReader(reader, codewire.MaxBody+1))
		if readErr != nil || len(body) > codewire.MaxBody {
			return codemode.Refuse(502, "code_upstream_invalid")
		}
		a.observe(a.observer.Unary(body))
		rewritten, rewriteErr := codewire.RewriteResponseModel(body, a.model, a.publicModel)
		if rewriteErr != nil {
			return rewriteErr
		}
		if deliver(rewritten) {
			a.finishCodeStream(ctx)
		}
		return nil
	}
	decoder := sse.NewRawDecoder(reader, codewire.MaxBody)
	for {
		frame, readErr := decoder.Next()
		if readErr == io.EOF {
			a.finishCodeStream(ctx)
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if frame.HasData {
			a.observe(a.observer.Event(frame.Data))
		}
		body := func() []byte {
			rewritten, rewriteErr := codewire.RewriteEventFrame(frame, a.endpoint.Protocol, a.model, a.publicModel)
			if rewriteErr != nil {
				return frame.Bytes
			}
			return rewritten
		}()
		if !deliver(body) {
			return nil
		}
	}
}

func (a *codeAttempt) finishCodeStream(ctx context.Context) {
	if !a.terminal {
		if observation, ok := a.observer.Finish(); ok {
			a.observe(observation)
		}
	}
	if !a.terminal && ctx.Err() == nil {
		a.outcome(ctx, codemode.Outcome{Origin: "upstream", Kind: "incomplete"})
	}
}

func codeModelEvent(body []byte, protocol codemode.Protocol, from, to string) []byte {
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return body
	}
	rewritten, err := codewire.RewriteEventModel(body, protocol, from, to)
	if err != nil {
		return body
	}
	return rewritten
}
