package gateway

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
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

func codeClosePeer(peer *websocket.Conn, err error) {
	if close, ok := errors.AsType[websocket.CloseError](err); ok {
		_ = peer.Close(close.Code, close.Reason)
	}
}

func (s *Server) codeWebSocket(w http.ResponseWriter, r *http.Request, release *runtime.Release, route codemode.Route, authority access.Authority) {
	keyID, digest := authority.ID, authority.EndUserDigest
	reject := func(err error) { s.codeRefuse(w, r, route, keyID, digest, err) }
	if r.Header.Get("Sec-Websocket-Protocol") != "" {
		reject(codemode.Refuse(400, "code_protocol_unsupported"))
		return
	}
	identity, err := codexwire.ConnectionIdentity(r.Header)
	if err != nil {
		reject(err)
		return
	}
	models := r.Header.Values("X-OLP-Code-Model")
	if len(models) > 1 || len(models) == 1 && strings.TrimSpace(models[0]) == "" {
		reject(codemode.Refuse(400, "code_model_invalid"))
		return
	}
	permit, err := s.CodeLedger.BindConnection(r.Context(), route, keyID, identity, r.Header.Get("X-OLP-Code-Model"), release.Snapshot.CodeProviders(route, "responses"))
	if err != nil {
		reject(err)
		return
	}
	if err := bindCodeWorkload(&permit.Authority, authority); err != nil {
		reject(err)
		return
	}
	labels, failure := s.parseAttribution(r, permit.Authority)
	if failure != nil {
		reject(codemode.Refuse(failure.Status, "code_"+failure.Code))
		return
	}
	if failure := s.checkKeyAddress(r, permit.Authority); failure != nil {
		reject(codemode.Refuse(failure.Status, "code_ip_not_allowed"))
		return
	}
	config, ok := release.Snapshot.CodeConnection(route, permit.Account.ProviderID)
	if !ok {
		reject(codemode.Refuse(503, "code_connection_unpublished"))
		return
	}
	auth, err := s.CodeAuthorizer.AuthorizeCode(r.Context(), config, permit.Account, codemode.Dispatch{Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses})
	if err != nil || auth.Principal == "" || auth.Principal != permit.Pin.Principal {
		reject(codemode.Refuse(503, "code_account_unavailable"))
		return
	}
	connection := &codeAttempt{server: s, permit: permit, config: config, auth: auth, allowance: true}
	httpClient, err := s.codeClient(r.Context(), release, config, permit.Account.ProviderID)
	if err != nil {
		reject(err)
		return
	}
	defer httpClient.CloseIdleConnections()
	target, err := s.codeEndpoint(config, "responses", r.URL.RawQuery)
	if err != nil {
		reject(err)
		return
	}
	headers := connection.headers(r.Header)
	for _, name := range []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		headers.Del(name)
	}
	forwarded := false
	httpClient.Transport = &codeHandshakeTransport{base: httpClient.Transport, reject: func(response *http.Response) {
		forwarded = true
		maps.Copy(w.Header(), codewire.ForwardHeaders(response.Header, false))
		for name := range codewire.ForwardTrailers(response.Trailer, response.Header, false) {
			w.Header().Add("Trailer", name)
		}
		w.WriteHeader(response.StatusCode)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		_, _ = io.Copy(w, response.Body)
		for name, values := range codewire.ForwardTrailers(response.Trailer, response.Header, false) {
			w.Header()[http.TrailerPrefix+name] = values
		}
	}}
	dialCtx, dialCancel := context.WithTimeout(r.Context(), 30*time.Second)
	upstream, response, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
	dialCancel()
	if response != nil {
		connection.response(r.Context(), response.StatusCode, response.Header)
	}
	if err != nil {
		if !forwarded {
			reject(codemode.Refuse(502, "code_upstream_unavailable"))
		}
		return
	}
	defer upstream.CloseNow()
	responseHeaders := codewire.ForwardHeaders(response.Header, false)
	for _, name := range []string{"Sec-Websocket-Accept", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		responseHeaders.Del(name)
	}
	maps.Copy(w.Header(), responseHeaders)
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(bodylimit.Lower(s.codeBodyLimit(), route.MaxBodyBytes))
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	clientMessages := codeRead(ctx, client)
	upstream.SetReadLimit(codewire.MaxBody)
	upstreamMessages := codeRead(ctx, upstream)
	var active *codeAttempt
	defer func() {
		if active != nil {
			active.finish(ctx)
		}
	}()
	account := permit.Account.ID
	var generationDone <-chan time.Time
	var generationTimer *time.Timer
	defer func() {
		if generationTimer != nil {
			generationTimer.Stop()
		}
	}()
	refuse := func(err error) {
		if active != nil {
			active.outcome(ctx, codemode.Outcome{Origin: "gateway", Kind: "rejected"})
		}
		refusal := codeRefusal(err)
		s.recordCodeRefusal(ctx, route, keyID, digest, refusal.Code)
		_ = client.Close(websocket.StatusPolicyViolation, refusal.Code)
	}
	var authorityTick <-chan time.Time
	if authority.WorkloadIssuerID != nil {
		ticker := time.NewTicker(realtimeReauth)
		defer ticker.Stop()
		authorityTick = ticker.C
	}
	for {
		select {
		case <-authorityTick:
			fresh, problem := s.authenticate(r, "inference")
			if problem != nil || fresh.ID != keyID || fresh.EndUserDigest != digest || !fresh.Allows("inference", route.Slug, &route.ProjectID, s.now()) || !fresh.AllowsAttribution(labels) {
				refuse(codemode.Refuse(403, "code_authority_revoked"))
				return
			}
		case <-ctx.Done():
			return
		case <-generationDone:
			return
		case message, open := <-clientMessages:
			if !open || message.err != nil {
				if active != nil {
					active.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
				}
				codeClosePeer(upstream, message.err)
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
			active, err = s.prepareCode(r.WithContext(ctx), release, route, observation, "responses", codemode.ProtocolResponses)
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
			// The socket still uses the authorization sent at its handshake,
			// even if this generation's admission reads a refreshed credential.
			active.auth = connection.auth
			if s.Runtime.Eligibility(connection.auth.CredentialID) != runtime.Eligible {
				refuse(codemode.Refuse(503, "code_account_unavailable"))
				return
			}
			if network := connection.config.Options.Network; network != nil && network.CredentialID != "" && s.Runtime.Eligibility(network.CredentialID) != runtime.Eligible {
				refuse(codemode.Refuse(503, "code_network_credential_unavailable"))
				return
			}
			if err := active.dispatch(ctx); err != nil {
				refuse(err)
				return
			}
			if err := codeWrite(ctx, upstream, message.kind, message.body); err != nil {
				active.transportFailed(ctx, err)
				return
			}
			generationTimer = time.NewTimer(codeGenerationTimeout)
			generationDone = generationTimer.C
		case message, open := <-upstreamMessages:
			if !open {
				return
			}
			if message.err != nil {
				if active != nil {
					active.transportFailed(ctx, message.err)
				}
				codeClosePeer(client, message.err)
				return
			}
			observation := codexwire.Observe(message.body, false)
			if active != nil {
				active.observe(observation)
			} else if observation.Allowance != nil {
				connection.observeAllowance(ctx, *observation.Allowance)
			}
			if err := codeWrite(ctx, client, message.kind, message.body); err != nil {
				if active != nil {
					active.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
				}
				return
			}
			if observation.Terminal && active != nil {
				active.finish(ctx)
				active = nil
				generationTimer.Stop()
				generationDone = nil
			}
		}
	}
}

type codeHandshakeTransport struct {
	base   http.RoundTripper
	reject func(*http.Response)
}

func (t *codeHandshakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && response.StatusCode != http.StatusSwitchingProtocols {
		t.reject(response)
		response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(""))
	}
	return response, err
}
