package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/runtime"
)

type codeMessage struct {
	kind websocket.MessageType
	body []byte
	err  error
}

func codeRead(ctx context.Context, conn *websocket.Conn) <-chan codeMessage {
	ch := make(chan codeMessage)
	go func() {
		defer close(ch)
		for {
			kind, body, err := conn.Read(ctx)
			select {
			case ch <- codeMessage{kind, body, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func codeWrite(ctx context.Context, conn *websocket.Conn, kind websocket.MessageType, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, responseWriteTimeout)
	defer cancel()
	return conn.Write(ctx, kind, body)
}

func (f *CodeForwarder) websocket(s *Server, w http.ResponseWriter, r *http.Request, release *runtime.Release, route codemode.Route) {
	if r.Header.Get("Sec-Websocket-Protocol") != "" {
		codeWriteError(w, codemode.Refuse(400, "code_protocol_unsupported"))
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(codexwire.MaxBody)
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	clientMessages := codeRead(ctx, client)
	var upstream *websocket.Conn
	defer func() {
		if upstream != nil {
			_ = upstream.CloseNow()
		}
	}()
	var upstreamMessages <-chan codeMessage
	var active *codeAttempt
	defer func() {
		if active != nil {
			active.finish(ctx)
		}
	}()
	var identity codemode.Identity
	var account string
	var generationDone <-chan time.Time
	var generationTimer *time.Timer
	defer func() {
		if generationTimer != nil {
			generationTimer.Stop()
		}
	}()
	responses := map[string]bool{}
	refuse := func(err error) {
		var refusal *codemode.Refusal
		if !errors.As(err, &refusal) {
			refusal = &codemode.Refusal{Code: "code_service_unavailable"}
		}
		authority, _ := s.authenticate(r, "inference")
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_ = s.CodeLedger.RecordRefusal(finishCtx, route, authority.ID, refusal.Code)
		finishCancel()
		_ = client.Close(websocket.StatusPolicyViolation, refusal.Code)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-generationDone:
			return
		case message, open := <-clientMessages:
			if !open || message.err != nil {
				return
			}
			if message.kind != websocket.MessageText {
				refuse(codemode.Refuse(400, "code_operation_unsupported"))
				return
			}
			if active != nil {
				refuse(codemode.Refuse(409, "code_generation_pending"))
				return
			}
			observation, err := codexwire.Classify(message.body, r.Header, "responses", true)
			if err != nil {
				refuse(err)
				return
			}
			if identity.Conversation != "" && observation.Operation.Identity != identity {
				refuse(codemode.Refuse(409, "code_identity_changed"))
				return
			}
			if observation.PreviousResponse != "" && !responses[observation.PreviousResponse] {
				refuse(codemode.Refuse(409, "code_parent_unresolved"))
				return
			}
			active, err = f.prepare(s, r.WithContext(ctx), release, route, observation)
			if err != nil {
				refuse(err)
				return
			}
			if account != "" && active.permit.Account.ID != account {
				refuse(codemode.Refuse(409, "code_pin_changed"))
				return
			}
			identity = observation.Operation.Identity
			account = active.permit.Account.ID
			if upstream == nil {
				client, err := f.client(s, active.config)
				if err != nil {
					refuse(err)
					return
				}
				defer client.CloseIdleConnections()
				target, err := codeEndpoint(s, active.config, "responses")
				if err != nil {
					refuse(err)
					return
				}
				headers := active.headers(r.Header)
				for _, name := range []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
					headers.Del(name)
				}
				if err := active.dispatch(ctx); err != nil {
					refuse(err)
					return
				}
				dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
				var response *http.Response
				upstream, response, err = websocket.Dial(dialCtx, target, &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
				dialCancel()
				if response != nil {
					active.response(ctx, response.StatusCode, response.Header)
				}
				if err != nil {
					if ctx.Err() == nil && response == nil {
						active.health(ctx, "unavailable")
					}
					refuse(codemode.Refuse(502, "code_upstream_unavailable"))
					return
				}
				upstream.SetReadLimit(codexwire.MaxBody)
				upstreamMessages = codeRead(ctx, upstream)
			} else {
				if err := active.dispatch(ctx); err != nil {
					refuse(err)
					return
				}
			}
			if err := codeWrite(ctx, upstream, message.kind, message.body); err != nil {
				return
			}
			generationTimer = time.NewTimer(codeGenerationTimeout)
			generationDone = generationTimer.C
		case message, open := <-upstreamMessages:
			if !open {
				return
			}
			if message.err != nil {
				status := websocket.CloseStatus(message.err)
				if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
					_ = client.Close(status, "")
				}
				return
			}
			observation := codexwire.Observe(message.body, false)
			if active != nil {
				active.observe(observation)
			}
			if err := codeWrite(ctx, client, message.kind, message.body); err != nil {
				return
			}
			if observation.Terminal && active != nil {
				if observation.ResponseID != "" && len(responses) < 4096 {
					responses[observation.ResponseID] = true
				}
				active.finish(ctx)
				active = nil
				generationTimer.Stop()
				generationDone = nil
			}
		}
	}
}
