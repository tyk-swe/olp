package gateway

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
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

type codeWebSocketHandshake struct {
	identity   codemode.Identity
	labels     map[string]string
	connection *codeAttempt
	client     *http.Client
	target     string
}

func (s *Server) prepareCodeWebSocket(r *http.Request, release *runtime.Release, route codemode.Route, authority access.Authority, ingress codeIngress) (codeWebSocketHandshake, error) {
	var handshake codeWebSocketHandshake
	identity, err := codeConnectionIdentity(ingress, r.Header)
	if err != nil {
		return handshake, err
	}
	handshake.identity = identity
	models := r.Header.Values("X-OLP-Code-Model")
	if len(models) > 1 || len(models) == 1 && strings.TrimSpace(models[0]) == "" {
		return handshake, codemode.Refuse(400, "code_model_invalid")
	}
	permit, err := s.CodeLedger.BindConnection(r.Context(), route, authority.ID, identity, r.Header.Get("X-OLP-Code-Model"), release.Snapshot.CodeProviders(route, ingress.path, codeadapter.WebSocket))
	if err != nil {
		return handshake, err
	}
	if err := bindCodeWorkload(&permit.Authority, authority); err != nil {
		return handshake, err
	}
	labels, failure := s.parseAttribution(r, permit.Authority)
	if failure != nil {
		return handshake, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	handshake.labels = labels
	if failure := s.checkKeyAddress(r, permit.Authority); failure != nil {
		return handshake, codemode.Refuse(failure.Status, "code_ip_not_allowed")
	}
	target, err := resolveCodeTarget(release.Snapshot, route, permit.Account.ProviderID, ingress, codeadapter.WebSocket)
	if err != nil {
		return handshake, err
	}
	auth, err := s.CodeAuthorizer.AuthorizeCode(r.Context(), target.config, permit.Account, target.dispatchDescriptor())
	if err != nil {
		return handshake, codemode.Refuse(503, "code_account_unavailable")
	}
	if err := target.adapter.ValidateAuthorization(target.endpoint, auth, permit.Pin.Principal); err != nil {
		// Keep the handshake's existing principal-refusal envelope.
		if codeRefusal(err).Code == "code_principal_mismatch" {
			err = codemode.Refuse(503, "code_account_unavailable")
		}
		return handshake, err
	}
	handshake.connection = &codeAttempt{server: s, permit: permit, config: target.config, auth: auth, endpoint: target.endpoint, observer: codeObserver(target.endpoint)}
	handshake.client, err = s.codeClient(r.Context(), release, target.config, permit.Account.ProviderID)
	if err != nil {
		return handshake, err
	}
	handshake.target, err = s.codeEndpoint(target.config, target.endpoint.UpstreamPath, r.URL.RawQuery)
	if err != nil {
		handshake.client.CloseIdleConnections()
		return handshake, err
	}
	return handshake, nil
}

func (s *Server) codeWebSocket(w http.ResponseWriter, r *http.Request, release *runtime.Release, route codemode.Route, authority access.Authority, ingress codeIngress) {
	keyID, digest := authority.ID, authority.EndUserDigest
	reject := func(err error) { s.codeRefuse(w, r, route, keyID, digest, err) }
	if r.Header.Get("Sec-Websocket-Protocol") != "" {
		reject(codemode.Refuse(400, "code_protocol_unsupported"))
		return
	}
	handshake, err := s.prepareCodeWebSocket(r, release, route, authority, ingress)
	if err != nil {
		reject(err)
		return
	}
	identity, labels, connection, httpClient, target := handshake.identity, handshake.labels, handshake.connection, handshake.client, handshake.target
	defer httpClient.CloseIdleConnections()
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
	account := connection.permit.Account.ID
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
			observation, err := classifyCode(ingress, message.body, r.Header, true)
			if err != nil {
				refuse(err)
				return
			}
			if identity.Conversation != "" && observation.Operation.Identity != identity {
				refuse(codemode.Refuse(409, "code_identity_changed"))
				return
			}
			active, err = s.prepareCode(r.WithContext(ctx), release, route, observation, ingress, codeadapter.WebSocket)
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
			if s.Runtime.Eligibility(connection.auth.CredentialID) != routing.Eligible {
				refuse(codemode.Refuse(503, "code_account_unavailable"))
				return
			}
			if network := connection.config.Options.Network; network != nil && network.CredentialID != "" && s.Runtime.Eligibility(network.CredentialID) != routing.Eligible {
				refuse(codemode.Refuse(503, "code_network_credential_unavailable"))
				return
			}
			body, err := func() ([]byte, error) {
				if active.model != active.publicModel {
					return codewire.RewriteRequestModel(message.body, active.publicModel, active.model)
				}
				return message.body, nil
			}()
			if err != nil {
				refuse(err)
				return
			}
			if err := active.dispatch(ctx); err != nil {
				refuse(err)
				return
			}
			if err := codeWrite(ctx, upstream, message.kind, body); err != nil {
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
			observer := connection.observer
			if active != nil {
				observer = active.observer
			}
			observation := observer.Event(message.body)
			if active != nil {
				active.observe(observation)
			} else if observation.Allowance != nil {
				connection.observeAllowance(ctx, *observation.Allowance)
			}
			body := func() []byte {
				if active != nil && active.model != active.publicModel {
					return codeModelEvent(message.body, active.model, active.publicModel)
				}
				return message.body
			}()
			if err := codeWrite(ctx, client, message.kind, body); err != nil {
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
