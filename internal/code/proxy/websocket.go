package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/release"
)

type wsMessage struct {
	kind websocket.MessageType
	body []byte
	err  error
}

func readWS(ctx context.Context, conn *websocket.Conn) <-chan wsMessage {
	ch := make(chan wsMessage)
	go func() {
		defer close(ch)
		for {
			kind, body, err := conn.Read(ctx)
			select {
			case ch <- wsMessage{kind, body, err}:
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

func writeWS(ctx context.Context, conn *websocket.Conn, kind websocket.MessageType, body []byte) error {
	step, cancel := context.WithTimeout(ctx, responseWriteTimeout)
	defer cancel()
	return conn.Write(step, kind, body)
}

func closePeerWS(peer *websocket.Conn, err error) {
	if closeErr, ok := errors.AsType[websocket.CloseError](err); ok {
		_ = peer.Close(closeErr.Code, closeErr.Reason)
	}
}

type wsHandshake struct {
	identity   code.Identity
	labels     map[string]string
	connection *subscriptionAttempt
	client     *http.Client
	target     string
}

func (s *Server) prepareWebSocket(r *http.Request, rel *release.Release, route code.Route, authority keyauthority.Authority, ingress vendor.Endpoint) (wsHandshake, error) {
	var handshake wsHandshake
	identity, err := wire.ConnectionIdentity(ingress.Identity, r.Header)
	if err != nil {
		return handshake, err
	}
	handshake.identity = identity
	models := r.Header.Values("X-OLP-Code-Model")
	if len(models) > 1 || len(models) == 1 && strings.TrimSpace(models[0]) == "" {
		return handshake, code.Refuse(400, "code_model_invalid")
	}
	if model := r.Header.Get("X-OLP-Code-Model"); model != "" {
		resolved, err := route.ResolveModel(code.Operation{Model: model})
		if err != nil {
			return handshake, err
		}
		if _, paid := route.PaidTargets[resolved.Model]; paid {
			return handshake, code.Refuse(400, "code_paid_operation_unsupported")
		}
	}
	permit, err := s.Ledger.BindConnection(r.Context(), route, authority.ID, identity, r.Header.Get("X-OLP-Code-Model"), rel.Snapshot().CodeProviders(route, ingress.ClientPath, code.WebSocket))
	if err != nil {
		return handshake, err
	}
	var operation code.Operation
	if len(models) > 0 {
		operation, err = route.ResolvePinnedModel(code.Operation{Model: models[0]}, []code.Pin{permit.Pin})
		if err != nil {
			return handshake, err
		}
	}
	if err := BindWorkload(&permit.Authority, authority); err != nil {
		return handshake, err
	}
	labels, failStatus, failCode, ok := s.Host.ParseAttribution(r, permit.Authority)
	if !ok {
		return handshake, code.Refuse(failStatus, "code_"+failCode)
	}
	handshake.labels = labels
	if failStatus, allowed := s.Host.CheckKeyAddress(r, permit.Authority); !allowed {
		return handshake, code.Refuse(failStatus, "code_ip_not_allowed")
	}
	cfg, v, endpoint, err := resolveSubscriptionTarget(rel.Snapshot(), route, permit.Account.ProviderID, ingress.ClientPath, code.WebSocket)
	if err != nil {
		return handshake, err
	}
	auth, err := s.Authorizer.AuthorizeCode(r.Context(), cfg, permit.Account, code.Dispatch{Adapter: v.Adapter, Protocol: endpoint.Protocol, ClientPath: endpoint.ClientPath})
	if err != nil {
		return handshake, code.Refuse(503, "code_account_unavailable")
	}
	if err := v.ValidateAuthorization(endpoint, auth, permit.Pin.Principal); err != nil {
		if RefusalFromError(err).Code == "code_principal_mismatch" {
			err = code.Refuse(503, "code_account_unavailable")
		}
		return handshake, err
	}
	handshake.connection = &subscriptionAttempt{
		core: attemptCore{
			server: s, model: operation.Model, protocol: endpoint.Protocol,
			observer: wire.NewObserver(endpoint),
		},
		permit: permit, config: cfg, vendor: v, endpoint: endpoint, auth: auth,
		identityOriginator: v.CompatibilityOriginator(route),
	}
	handshake.client, err = s.Host.ConnectionClient(r.Context(), rel, cfg, permit.Account.ProviderID)
	if err != nil {
		return handshake, err
	}
	handshake.target, err = subscriptionEndpointURL(s.Host, cfg, endpoint.UpstreamPath, r.URL.RawQuery)
	if err != nil {
		handshake.client.CloseIdleConnections()
		return handshake, err
	}
	return handshake, nil
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request, rel *release.Release, route code.Route, authority keyauthority.Authority, ingress vendor.Endpoint) {
	keyID, digest := authority.ID, authority.EndUserDigest
	reject := func(err error) { s.refuse(w, r, route, keyID, digest, err) }
	if r.Header.Get("Sec-Websocket-Protocol") != "" {
		reject(code.Refuse(400, "code_protocol_unsupported"))
		return
	}
	handshake, err := s.prepareWebSocket(r, rel, route, authority, ingress)
	if err != nil {
		reject(err)
		return
	}
	identity, labels, connection, httpClient, target := handshake.identity, handshake.labels, handshake.connection, handshake.client, handshake.target
	defer httpClient.CloseIdleConnections()
	headers := buildUpstreamHeaders(r.Header, connection.auth.Headers, connection.identityOriginator)
	for _, name := range []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		headers.Del(name)
	}
	forwarded := false
	httpClient.Transport = &handshakeTransport{base: httpClient.Transport, reject: func(response *http.Response) {
		forwarded = true
		forwardHandshakeRejection(r.Context(), w, connection, response)
	}}
	dialCtx, dialCancel := context.WithTimeout(r.Context(), 30*time.Second)
	upstreamConn, response, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
	dialCancel()
	if response != nil && !forwarded {
		connection.onResponseHeaders(r.Context(), response.StatusCode, response.Header)
	}
	if err != nil {
		connection.flushPendingHealth(r.Context())
		if !forwarded {
			reject(code.Refuse(502, "code_upstream_unavailable"))
		}
		return
	}
	defer upstreamConn.CloseNow()
	responseHeaders := wire.ForwardHeaders(response.Header, false)
	for _, name := range []string{"Sec-Websocket-Accept", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		responseHeaders.Del(name)
	}
	maps.Copy(w.Header(), responseHeaders)
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer clientConn.CloseNow()
	clientConn.SetReadLimit(bodylimit.Lower(s.Host.BodyLimit(), route.MaxBodyBytes))
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	clientMessages := readWS(ctx, clientConn)
	upstreamConn.SetReadLimit(wire.MaxBody)
	upstreamMessages := readWS(ctx, upstreamConn)
	var active *subscriptionAttempt
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
			active.core.recordOutcome(ctx, code.GatewayOutcome(code.OutcomeRejected, s.Host.Now().UTC()))
		}
		refusal := RefusalFromError(err)
		s.recordRefusal(ctx, route, keyID, digest, refusal.Code)
		_ = clientConn.Close(websocket.StatusPolicyViolation, refusal.Code)
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
			fresh, _, ok := s.Host.Authenticate(r)
			if !ok || fresh.ID != keyID || fresh.EndUserDigest != digest || !fresh.Allows("inference", route.Slug, &route.ProjectID, s.Host.Now()) || !fresh.AllowsAttribution(labels) {
				refuse(code.Refuse(403, "code_authority_revoked"))
				return
			}
		case <-ctx.Done():
			return
		case <-generationDone:
			return
		case message, open := <-clientMessages:
			if !open || message.err != nil {
				if active != nil {
					active.core.recordOutcome(ctx, code.ClientCanceledOutcome(s.Host.Now().UTC()))
				}
				closePeerWS(upstreamConn, message.err)
				return
			}
			if message.kind != websocket.MessageText {
				refuse(code.Refuse(400, "code_operation_unsupported"))
				return
			}
			if active != nil {
				refuse(code.Refuse(409, "code_generation_pending"))
				return
			}
			observation, err := wire.ClassifyRequest(ingress, message.body, r.Header, true)
			if err != nil {
				refuse(err)
				return
			}
			if identity.Conversation != "" && observation.Operation.Identity != identity {
				refuse(code.Refuse(409, "code_identity_changed"))
				return
			}
			prepared, paid, err := s.prepareExecution(r.WithContext(ctx), rel, route, observation, ingress, code.WebSocket, nil, message.body)
			if paid != nil {
				paid.finish(ctx)
				refuse(code.Refuse(400, "code_paid_operation_unsupported"))
				return
			}
			if err != nil {
				refuse(err)
				return
			}
			active = prepared
			if account != "" && active.permit.Account.ID != account {
				refuse(code.Refuse(409, "code_pin_changed"))
				return
			}
			identity = observation.Operation.Identity
			account = active.permit.Account.ID
			active.auth = connection.auth
			if !s.Host.IsCredentialEligible(connection.auth.CredentialID) {
				refuse(code.Refuse(503, "code_account_unavailable"))
				return
			}
			if network := connection.config.Options.Network; network != nil && network.CredentialID != "" && !s.Host.IsCredentialEligible(network.CredentialID) {
				refuse(code.Refuse(503, "code_network_credential_unavailable"))
				return
			}
			body := message.body
			if active.core.model != active.core.publicModel {
				body, err = wire.RewriteRequestModel(message.body, active.core.publicModel, active.core.model)
				if err != nil {
					refuse(err)
					return
				}
			}
			if err := active.core.dispatch(ctx); err != nil {
				refuse(err)
				return
			}
			if err := writeWS(ctx, upstreamConn, message.kind, body); err != nil {
				active.transportFailed(ctx, err)
				return
			}
			generationTimer = time.NewTimer(GenerationTimeout)
			generationDone = generationTimer.C
		case message, open := <-upstreamMessages:
			if !open {
				return
			}
			if message.err != nil {
				if active != nil {
					active.transportFailed(ctx, message.err)
				}
				closePeerWS(clientConn, message.err)
				return
			}
			observer := connection.core.observer
			if active != nil {
				observer = active.core.observer
			}
			observation := observer.Event(message.body)
			if active != nil {
				active.observe(observation)
			} else if observation.Allowance != nil {
				connection.observeAllowance(ctx, *observation.Allowance)
			}
			body := message.body
			if active != nil && active.core.model != active.core.publicModel && bytes.HasPrefix(bytes.TrimSpace(message.body), []byte("{")) {
				if rewritten, err := wire.RewriteEventModel(message.body, active.core.protocol, active.core.model, active.core.publicModel); err == nil {
					body = rewritten
				}
			}
			if err := writeWS(ctx, clientConn, message.kind, body); err != nil {
				if active != nil {
					active.core.recordOutcome(ctx, code.ClientCanceledOutcome(s.Host.Now().UTC()))
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

func forwardHandshakeRejection(ctx context.Context, w http.ResponseWriter, attempt *subscriptionAttempt, response *http.Response) {
	attempt.onResponseHeaders(ctx, response.StatusCode, response.Header)
	writeUpstreamResponseHeaders(w, response, false)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	writer := flushingHandshakeWriter{w}
	body, readErr := io.ReadAll(io.TeeReader(io.LimitReader(response.Body, wire.MaxBody+1), writer))
	if readErr == nil {
		if _, err := io.Copy(writer, response.Body); err == nil && response.StatusCode >= 400 {
			attempt.observeEncodedUnary(body, response.Header.Get("Content-Encoding"))
		}
	}
	writeUpstreamResponseTrailers(w, response)
}

type flushingHandshakeWriter struct {
	http.ResponseWriter
}

func (w flushingHandshakeWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	if err == nil {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}

type handshakeTransport struct {
	base   http.RoundTripper
	reject func(*http.Response)
}

func (t *handshakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && response.StatusCode != http.StatusSwitchingProtocols {
		t.reject(response)
		response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(""))
	}
	return response, err
}
