package codeforward

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codewire"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
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
	ctx, cancel := context.WithTimeout(ctx, fault.ResponseWriteTimeout)
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

func (f *Forwarder) prepareCodeWebSocket(r *http.Request, release *release.Release, route codemode.Route, authority keyauthority.Authority, inbound codeIngress) (codeWebSocketHandshake, error) {
	var handshake codeWebSocketHandshake
	identity, err := codeConnectionIdentity(inbound.identity, r.Header)
	if err != nil {
		return handshake, err
	}
	handshake.identity = identity
	models := r.Header.Values("X-OLP-Code-Model")
	if len(models) > 1 || len(models) == 1 && strings.TrimSpace(models[0]) == "" {
		return handshake, codemode.Refuse(400, "code_model_invalid")
	}
	if model := r.Header.Get("X-OLP-Code-Model"); model != "" {
		resolved, err := route.ResolveModel(codemode.Operation{Model: model})
		if err != nil {
			return handshake, err
		}
		if _, paid := route.PaidTargets[resolved.Model]; paid {
			return handshake, codemode.Refuse(400, "code_paid_operation_unsupported")
		}
	}
	permit, err := f.ledger.BindConnection(r.Context(), route, authority.ID, identity, r.Header.Get("X-OLP-Code-Model"), release.Snapshot().CodeProviders(route, inbound.path, codeadapter.WebSocket))
	if err != nil {
		return handshake, err
	}
	operation, err := func() (codemode.Operation, error) {
		if len(models) == 0 {
			return codemode.Operation{}, nil
		}
		return route.ResolvePinnedModel(codemode.Operation{Model: models[0]}, []codemode.Pin{permit.Pin})
	}()
	if err != nil {
		return handshake, err
	}
	if err := bindCodeWorkload(&permit.Authority, authority); err != nil {
		return handshake, err
	}
	labels, failure := ingress.ParseAttribution(r, permit.Authority)
	if failure != nil {
		return handshake, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	handshake.labels = labels
	if failure := f.gate.CheckKeyAddress(r, permit.Authority); failure != nil {
		return handshake, codemode.Refuse(failure.Status, "code_ip_not_allowed")
	}
	target, err := resolveCodeTarget(release.Snapshot(), route, permit.Account.ProviderID, inbound, codeadapter.WebSocket)
	if err != nil {
		return handshake, err
	}
	auth, err := f.authorizer.AuthorizeCode(r.Context(), target.config, permit.Account, target.dispatchDescriptor())
	if err != nil {
		return handshake, codemode.Refuse(503, "code_account_unavailable")
	}
	if err := target.adapter.ValidateAuthorization(target.endpoint, auth, permit.Pin.Principal); err != nil {
		// Keep the handshake'f existing principal-refusal envelope.
		if codeRefusal(err).Code == "code_principal_mismatch" {
			err = codemode.Refuse(503, "code_account_unavailable")
		}
		return handshake, err
	}
	handshake.connection = &codeAttempt{model: operation.Model, identityOriginator: target.adapter.CompatibilityOriginator(route), adapter: target.adapter.Adapter, forwarder: f, permit: permit, config: target.config, auth: auth, endpoint: target.endpoint, observer: codeObserver(target.endpoint, 1)}
	handshake.client, err = f.codeClient(r.Context(), release, target.config, permit.Account.ProviderID)
	if err != nil {
		return handshake, err
	}
	handshake.target, err = f.codeEndpoint(target.config, target.endpoint.UpstreamPath, r.URL.RawQuery)
	if err != nil {
		handshake.client.CloseIdleConnections()
		return handshake, err
	}
	return handshake, nil
}

func (f *Forwarder) codeWebSocket(w http.ResponseWriter, r *http.Request, release *release.Release, route codemode.Route, authority keyauthority.Authority, inbound codeIngress) {
	reject := func(err error) { f.codeRefuse(w, r, route, authority.ID, authority.EndUserDigest, err) }
	if r.Header.Get("Sec-Websocket-Protocol") != "" {
		reject(codemode.Refuse(400, "code_protocol_unsupported"))
		return
	}
	handshake, err := f.prepareCodeWebSocket(r, release, route, authority, inbound)
	if err != nil {
		reject(err)
		return
	}
	defer handshake.client.CloseIdleConnections()
	upstream, client, ok := f.connectCodeWebSocket(w, r, route, handshake, reject)
	if !ok {
		return
	}
	defer upstream.CloseNow()
	defer client.CloseNow()
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	loop := &codeWSLoop{
		f: f, r: r, release: release, route: route, authority: authority, inbound: inbound,
		handshake: handshake, client: client, upstream: upstream,
		identity: handshake.identity, account: handshake.connection.permit.Account.ID,
	}
	defer loop.cleanup(ctx)
	loop.run(ctx)
}

func (f *Forwarder) connectCodeWebSocket(w http.ResponseWriter, r *http.Request, route codemode.Route, handshake codeWebSocketHandshake, reject func(error)) (*websocket.Conn, *websocket.Conn, bool) {
	connection, httpClient := handshake.connection, handshake.client
	headers := connection.headers(r.Header)
	for _, name := range []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		headers.Del(name)
	}
	forwarded := false
	httpClient.Transport = &codeHandshakeTransport{base: httpClient.Transport, reject: func(response *http.Response) {
		forwarded = true
		forwardCodeHandshakeRejection(r.Context(), w, connection, response)
	}}
	dialCtx, dialCancel := context.WithTimeout(r.Context(), 30*time.Second)
	upstream, response, err := websocket.Dial(dialCtx, handshake.target, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
	dialCancel()
	if response != nil && !forwarded {
		connection.response(r.Context(), response.StatusCode, response.Header)
	}
	if err != nil {
		connection.flushPendingHealth(r.Context())
		if !forwarded {
			reject(codemode.Refuse(502, "code_upstream_unavailable"))
		}
		return nil, nil, false
	}
	responseHeaders := codewire.ForwardHeaders(response.Header, false)
	for _, name := range []string{"Sec-Websocket-Accept", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		responseHeaders.Del(name)
	}
	maps.Copy(w.Header(), responseHeaders)
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		_ = upstream.CloseNow()
		return nil, nil, false
	}
	client.SetReadLimit(bodylimit.Lower(f.codeBodyLimit(), route.MaxBodyBytes))
	upstream.SetReadLimit(codewire.MaxBody)
	return upstream, client, true
}

type codeWSLoop struct {
	f              *Forwarder
	r              *http.Request
	release        *release.Release
	route          codemode.Route
	authority      keyauthority.Authority
	inbound        codeIngress
	handshake      codeWebSocketHandshake
	client         *websocket.Conn
	upstream       *websocket.Conn
	stopGeneration context.CancelFunc
	active         *codeAttempt
	identity       codemode.Identity
	account        string
}

func (l *codeWSLoop) cleanup(ctx context.Context) {
	if l.stopGeneration != nil {
		l.stopGeneration()
		l.stopGeneration = nil
	}
	if l.active != nil {
		_ = l.upstream.CloseNow()
		l.active.finish(ctx)
	}
}

func (l *codeWSLoop) refuse(ctx context.Context, err error) {
	if l.active != nil {
		l.active.outcome(ctx, codemode.Outcome{Origin: "gateway", Kind: "rejected"})
	}
	refusal := codeRefusal(err)
	l.f.recordCodeRefusal(ctx, l.route, l.authority.ID, l.authority.EndUserDigest, refusal.Code)
	if refusal.Code == "code_account_busy" {
		_ = l.client.Close(websocket.StatusTryAgainLater, refusal.Code)
		return
	}
	_ = l.client.Close(websocket.StatusPolicyViolation, refusal.Code)
}

func (l *codeWSLoop) run(ctx context.Context) {
	clientMessages := codeRead(ctx, l.client)
	upstreamMessages := codeRead(ctx, l.upstream)
	var authorityTick <-chan time.Time
	if l.authority.WorkloadIssuerID != nil {
		ticker := time.NewTicker(ingress.ReauthInterval)
		defer ticker.Stop()
		authorityTick = ticker.C
	}
	genCtx := ctx
	var generationDone <-chan struct{}
	for {
		select {
		case <-authorityTick:
			fresh, problem := l.f.gate.Authenticate(l.r, "inference")
			if problem != nil || fresh.ID != l.authority.ID || fresh.EndUserDigest != l.authority.EndUserDigest || !fresh.Allows("inference", l.route.Slug, &l.route.ProjectID, l.f.now()) || !fresh.AllowsAttribution(l.handshake.labels) {
				l.refuse(ctx, codemode.Refuse(403, "code_authority_revoked"))
				return
			}
		case <-ctx.Done():
			return
		case <-generationDone:
			return
		case message, open := <-clientMessages:
			nextGen, ok := l.handleClient(ctx, message, open)
			if !ok {
				return
			}
			genCtx, generationDone = nextGen, nextGen.Done()
		case message, open := <-upstreamMessages:
			finished, ok := l.handleUpstream(ctx, genCtx, message, open)
			if !ok {
				return
			}
			if finished {
				genCtx, generationDone = ctx, nil
			}
		}
	}
}

func (l *codeWSLoop) handleClient(ctx context.Context, message codeMessage, open bool) (context.Context, bool) {
	if !open || message.err != nil {
		if l.active != nil {
			l.active.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
		}
		codeClosePeer(l.upstream, message.err)
		return nil, false
	}
	if message.kind != websocket.MessageText {
		l.refuse(ctx, codemode.Refuse(400, "code_operation_unsupported"))
		return nil, false
	}
	if l.active != nil {
		l.refuse(ctx, codemode.Refuse(409, "code_generation_pending"))
		return nil, false
	}
	observation, err := classifyCode(l.inbound, message.body, l.r.Header, true)
	if err != nil {
		l.refuse(ctx, err)
		return nil, false
	}
	if l.identity.Conversation != "" && observation.Operation.Identity != l.identity {
		l.refuse(ctx, codemode.Refuse(409, "code_identity_changed"))
		return nil, false
	}
	genCtx, stop := context.WithTimeout(ctx, codeGenerationTimeout)
	l.stopGeneration = stop
	l.active, err = l.f.prepareCode(codeRequest{
		r: l.r.WithContext(genCtx), release: l.release, route: l.route, observation: observation,
		inbound: l.inbound, transport: codeadapter.WebSocket, body: message.body,
	}, nil)
	if err != nil {
		l.refuse(ctx, err)
		return nil, false
	}
	if l.account != "" && l.active.permit.Account.ID != l.account {
		l.refuse(ctx, codemode.Refuse(409, "code_pin_changed"))
		return nil, false
	}
	l.identity, l.account = observation.Operation.Identity, l.active.permit.Account.ID
	connection := l.handshake.connection
	l.active.auth = connection.auth
	if l.f.runtime.Eligibility(connection.auth.CredentialID) != routing.Eligible {
		l.refuse(ctx, codemode.Refuse(503, "code_account_unavailable"))
		return nil, false
	}
	if network := connection.config.Options.Network; network != nil && network.CredentialID != "" && l.f.runtime.Eligibility(network.CredentialID) != routing.Eligible {
		l.refuse(ctx, codemode.Refuse(503, "code_network_credential_unavailable"))
		return nil, false
	}
	body := message.body
	if l.active.model != l.active.publicModel {
		body, err = codewire.RewriteRequestModel(message.body, l.active.publicModel, l.active.model)
		if err != nil {
			l.refuse(ctx, err)
			return nil, false
		}
	}
	if err := l.active.dispatch(genCtx); err != nil {
		l.refuse(ctx, err)
		return nil, false
	}
	if err := codeWrite(genCtx, l.upstream, message.kind, body); err != nil {
		l.active.transportFailed(ctx, err)
		return nil, false
	}
	return genCtx, true
}

func (l *codeWSLoop) handleUpstream(ctx, genCtx context.Context, message codeMessage, open bool) (finished bool, ok bool) {
	if !open {
		return false, false
	}
	if message.err != nil {
		if l.active != nil {
			l.active.transportFailed(ctx, message.err)
		}
		codeClosePeer(l.client, message.err)
		return false, false
	}
	observer := l.handshake.connection.observer
	if l.active != nil {
		observer = l.active.observer
	}
	observation := observer.Event(message.body)
	if l.active != nil {
		l.active.observe(ctx, observation)
	} else if observation.Allowance != nil {
		l.handshake.connection.observeAllowance(ctx, *observation.Allowance)
	}
	body := message.body
	if l.active != nil && l.active.model != l.active.publicModel {
		body = codeModelEvent(message.body, l.active.endpoint.Protocol, l.active.model, l.active.publicModel)
	}
	if err := codeWrite(genCtx, l.client, message.kind, body); err != nil {
		if l.active != nil {
			l.active.outcome(ctx, codemode.Outcome{Origin: "client", Kind: "canceled"})
		}
		return false, false
	}
	if observation.Terminal && l.active != nil {
		l.active.finish(ctx)
		l.active = nil
		l.stopGeneration()
		l.stopGeneration = nil
		return true, true
	}
	return false, true
}

func forwardCodeHandshakeRejection(ctx context.Context, w http.ResponseWriter, attempt *codeAttempt, response *http.Response) {
	attempt.response(ctx, response.StatusCode, response.Header)
	maps.Copy(w.Header(), codewire.ForwardHeaders(response.Header, false))
	for name := range codewire.ForwardTrailers(response.Trailer, response.Header, false) {
		w.Header().Add("Trailer", name)
	}
	w.WriteHeader(response.StatusCode)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	writer := flushingHandshakeWriter{w}
	body, readErr := io.ReadAll(io.TeeReader(io.LimitReader(response.Body, codewire.MaxBody+1), writer))
	if readErr == nil {
		if _, err := io.Copy(writer, response.Body); err == nil && response.StatusCode >= 400 {
			attempt.observeEncodedUnary(ctx, body, response.Header.Get("Content-Encoding"))
		}
	}
	for name, values := range codewire.ForwardTrailers(response.Trailer, response.Header, false) {
		w.Header()[http.TrailerPrefix+name] = values
	}
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
