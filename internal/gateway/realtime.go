package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/usage"
)

const realtimeSession = time.Hour

const realtimeReauth = 5 * time.Second

const realtimePing = 30 * time.Second

func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	x := &execution{request: s.begin(w, r), family: openai.FamilyRealtime, actor: "api_key"}
	x.mode = "realtime"
	fail := func(e *Error) {
		x.failure = e
		s.finish(x, nil, e.Status)
		writeSurfaceError(w, e, "openai")
	}
	if !s.admit(r.Context()) {
		fail(overloaded)
		return
	}
	defer s.release(r.Context())

	if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(s.cfg.CORSAllowedOrigins, origin) {
		fail(permissionError("origin_forbidden", "This origin is not allowed to open realtime sessions."))
		return
	}

	if !realtimeUpgrade(r) {
		fail(invalidRequest("websocket_upgrade_required", "This endpoint requires a WebSocket upgrade request.", nil))
		return
	}
	token := bearerToken(r)
	if token == "" {
		fail(authenticationError("missing_authorization", "Provide an Authorization bearer API key."))
		return
	}
	authority, e := s.authenticate(r, "inference")
	if e != nil {
		fail(e)
		return
	}
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.budgetGroupID = authority.BudgetGroupID
	attributionValues := r.Header.Values(usage.AttributionHeader)
	if len(attributionValues) == 0 {
		attributionValues = r.URL.Query()["attribution"]
	}
	if x.attribution, e = parseAttributionValues(attributionValues, authority); e != nil {
		fail(e)
		return
	}
	slug := r.URL.Query().Get("model")
	if !openai.RouteSlug.MatchString(slug) {
		fail(invalidRequest("invalid_value", "model must name a published route slug.", strPtr("model")))
		return
	}
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[slug]
	if !ok {
		fail(modelNotFound(slug))
		return
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, s.now()) {
		fail(permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	if !slices.Contains(route.Operations, "realtime") {
		fail(invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow realtime sessions.", nil))
		return
	}
	if e := policySurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	p, e := s.selectPin(r.Context(), x, &route, "realtime", "realtime")
	if e != nil {
		fail(e)
		return
	}
	defer s.resourceSettle(r.Context(), x, p)
	if e := s.reserveState(r.Context(), x, authority, realtimeSession); e != nil {
		fail(e)
		return
	}
	upstream, e := realtimeURL(p)
	if e != nil {
		fail(e)
		return
	}
	check := strings.Replace(upstream, "wss://", "https://", 1)
	check = strings.Replace(check, "ws://", "http://", 1)
	if u, e := url.Parse(check); e == nil {
		u.RawQuery, u.Fragment = "", ""
		check = u.String()
	}
	if _, err := s.egress.ValidateEndpoint(check); err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		fail(serverError(http.StatusBadRequest, "websocket_upgrade_failed", "The WebSocket upgrade could not be completed."))
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(r.Context(), realtimeSession)
	defer cancel()
	conn, e := realtimeDial(ctx, s, x, p, upstream)
	if e != nil {

		client.Close(websocket.StatusInternalError, "upstream unavailable")
		x.failure = e
		s.finish(x, nil, e.Status)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	limit := s.cfg.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	conn.SetReadLimit(limit)
	x.dispatched = true
	x.delivered(s.now())
	usage := s.relayRealtime(ctx, x, p, client, conn, token, authority.ID)
	if len(x.facts) > 0 {
		fact := &x.facts[len(x.facts)-1]
		if usage != nil {
			fact.Usage = usage
			fact.UsageObserved, fact.UsageComplete, fact.BillingUncertain = true, true, false
		} else {
			fact.UsageComplete = false
			fact.BillingUncertain = true
		}
	}
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func realtimeUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	upgrades := false
	for _, line := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				upgrades = true
			}
		}
	}
	if !upgrades || !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key")))
	return err == nil && len(key) == 16
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

func realtimeURL(p *pin) (string, *Error) {
	cfg := p.provider.Connector()
	base := strings.TrimRight(cfg.Endpoint, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return "", serverError(http.StatusBadGateway, "upstream_error", "The provider endpoint cannot serve realtime sessions.")
	}
	query := url.Values{}
	if cfg.Kind == "azure_openai" {
		deployment := cfg.Model(p.model)
		if deployment == "" {
			return "", serverError(http.StatusBadGateway, "upstream_error", "The model has no configured Azure deployment.")
		}
		query.Set("api-version", cfg.APIVersion)
		query.Set("deployment", deployment)
		return base + "/openai/realtime?" + query.Encode(), nil
	}
	query.Set("model", cfg.Model(p.model))
	return base + "/realtime?" + query.Encode(), nil
}

func realtimeDial(ctx context.Context, s *Server, x *execution, p *pin, endpoint string) (*websocket.Conn, *Error) {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	fact.Mode = "realtime"
	finish := func(class string, e *Error) *Error {
		fact.Class = class
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return e
	}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, finish(classConnect, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
	}
	var secret []byte
	if p.slot.CredentialID != nil {
		secret, _ = x.request.release.Credential(*p.slot.CredentialID)
	}
	if _, err := s.auth.Apply(ctx, probe, p.provider.Connector(), secret, nil); err != nil {
		return nil, finish(classCredential, serverError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied."))
	}
	headers := http.Header{}
	for name, values := range probe.Header {
		headers[name] = append([]string{}, values...)
	}
	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: s.client, HTTPHeader: headers})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		fact.Status = status
		class := classConnect
		switch {
		case ctx.Err() != nil:
			class = classCancelled
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			class = classCredential
		case status == http.StatusTooManyRequests:
			class = classRateLimit
		case status >= 500:
			class = classUpstreamServer
		case status != 0:
			class = classUpstreamClient
		}
		return nil, finish(class, upstreamError(&attemptFailure{status: status, upstream: upstreamResponseError(resp)}))
	}
	fact.Class = "success"
	fact.Committed = true
	fact.Duration = s.now().Sub(fact.StartedAt)
	fact.recordEvidence(true)
	x.facts = append(x.facts, fact)
	return conn, nil
}

func upstreamResponseError(resp *http.Response) *openai.UpstreamError {
	if resp == nil || resp.Body == nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if err != nil {
		return nil
	}
	return openai.ParseErrorBody(body)
}

func (s *Server) relayRealtime(ctx context.Context, x *execution, p *pin, client, conn *websocket.Conn, token, keyID string) *openai.Usage {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var usageMu sync.Mutex
	usage := &openai.Usage{}
	have := false
	forward := func(dst, src *websocket.Conn, inspect bool) error {
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				return err
			}
			if inspect {
				if u := realtimeUsage(data); u != nil {
					usageMu.Lock()
					usage.InputTokens += u.InputTokens
					usage.OutputTokens += u.OutputTokens
					if u.CachedInputTokens != nil {
						cached := *u.CachedInputTokens
						if usage.CachedInputTokens != nil {
							cached += *usage.CachedInputTokens
						}
						usage.CachedInputTokens = &cached
					}
					have = true
					usageMu.Unlock()
				}
			}
			if err := dst.Write(ctx, typ, data); err != nil {
				return err
			}
		}
	}
	done := make(chan error, 2)
	go func() { done <- forward(conn, client, false) }()
	go func() { done <- forward(client, conn, true) }()
	reauth := time.NewTicker(realtimeReauth)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	var first error
loop:
	for {
		select {
		case err := <-done:
			first = err
			break loop
		case <-reauth.C:
			authority, err := s.Runtime.Authenticate(token)
			if err != nil || authority.ID != keyID || !authority.Allows("inference", x.route.Slug, s.now()) {
				first = errors.New("key authority revoked")
				client.Close(websocket.StatusPolicyViolation, "key revoked")
				break loop
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, realtimeReauth)
			err := conn.Ping(pingCtx)
			if err == nil {
				err = client.Ping(pingCtx)
			}
			pingCancel()
			if err != nil {
				first = err
				break loop
			}
		case <-ctx.Done():
			first = ctx.Err()
			break loop
		}
	}
	cancel()

	code := websocket.CloseStatus(first)
	if code < 0 {
		code = websocket.StatusNormalClosure
	}
	conn.Close(code, "")
	client.Close(code, "")
	<-done
	usageMu.Lock()
	defer usageMu.Unlock()
	if !have {
		return nil
	}
	return usage
}

func realtimeUsage(data []byte) *openai.Usage {
	var event struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				InputDetails *struct {
					CachedTokens *int64 `json:"cached_tokens"`
				} `json:"input_token_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &event) != nil {
		return nil
	}
	if event.Type != "response.done" || event.Response == nil || event.Response.Usage == nil {
		return nil
	}
	usage := event.Response.Usage
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return nil
	}
	out := &openai.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
	if usage.InputDetails != nil && usage.InputDetails.CachedTokens != nil {
		cached := *usage.InputDetails.CachedTokens
		if cached < 0 || cached > usage.InputTokens {
			return nil
		}
		out.CachedInputTokens = &cached
	}
	return out
}

func strPtr(value string) *string { return &value }
