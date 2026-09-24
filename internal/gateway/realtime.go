package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/realtimecontract"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const realtimeSession = time.Hour

const realtimeReauth = 5 * time.Second

const realtimePing = 30 * time.Second

var errRealtimeAuthorityRevoked = errors.New("realtime authority revoked")
var errRealtimeProviderCredentialRevoked = errors.New("realtime provider credential revoked")
var errRealtimeClientClosed = errors.New("realtime client disconnected")
var errRealtimeResponseIncomplete = errors.New("realtime response ended before its terminal event")

// The wire relay does not buffer native frames. This small state machine only
// records whether a response is still owed when either peer closes normally.
// A provider-controlled ID set is bounded; an untrackable event conservatively
// makes a clean socket close incomplete instead of claiming terminal delivery.
const maxRealtimePendingResponses = 128
const maxRealtimeTrackedFrameBytes = 1 << 20

type realtimeResponseState struct {
	requested int
	active    map[string]struct{}
	anonymous bool
	unknown   bool
}

func (s *realtimeResponseState) pending() bool {
	return s.requested > 0 || len(s.active) > 0 || s.anonymous || s.unknown
}

func (s *realtimeResponseState) add(id string, strict bool) {
	if id == "" || len(id) > 512 {
		s.unknown = true
		return
	}
	if s.active == nil {
		s.active = make(map[string]struct{})
	}
	if _, found := s.active[id]; found {
		return
	}
	if len(s.active) >= maxRealtimePendingResponses {
		s.unknown = true
		return
	}
	if strict {
		// A strict decoder may return a view into the source frame. Keep
		// only the bounded identifier, never the audio-bearing frame.
		id = strings.Clone(id)
	}
	s.active[id] = struct{}{}
}

// realtimeObject validates the entire native frame, including unknown nested
// controls, without materializing a second JSON tree for the few observed
// response fields. OIF rejects duplicate keys and invalid Unicode throughout.
func realtimeObject(data []byte) (oif.Value, bool) {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxRealtimeTrackedFrameBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Value{}, false
	}
	return doc.Root(), true
}

// realtimeFlatFrame recognizes the common native audio/event shape without
// building an index for fields the relay never interprets. It accepts only a
// flat object with unescaped keys and string values; every other shape falls
// through to OIF. The fixed key table catches duplicate members, while
// json.Valid and UTF-8 validation cover the complete flat document. Escapes
// fall through so OIF still enforces surrogate-pair and decoded-key rules.
func realtimeFlatFrame(data []byte) (realtimeFrame, bool) {
	var event realtimeFrame
	if len(data) > maxRealtimeTrackedFrameBytes || !utf8.Valid(data) {
		return event, false
	}
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
		return event, false
	}
	var keys [16][]byte
	count, at := 0, 1
	space := func() {
		for at < len(data) && (data[at] == ' ' || data[at] == '\t' || data[at] == '\r' || data[at] == '\n') {
			at++
		}
	}
	for {
		space()
		if at >= len(data) {
			return realtimeFrame{}, false
		}
		if data[at] == '}' {
			at++
			break
		}
		if data[at] != '"' || count == len(keys) {
			return realtimeFrame{}, false
		}
		at++
		start := at
		for at < len(data) && data[at] != '"' {
			if data[at] == '\\' {
				return realtimeFrame{}, false
			}
			at++
		}
		if at >= len(data) {
			return realtimeFrame{}, false
		}
		key := data[start:at]
		// encoding/json's struct fields accept case-insensitive spellings and
		// assign the last matching member. Let the fallback preserve that rule;
		// response is structured even if a caller sends a scalar value.
		if bytes.EqualFold(key, []byte("response")) ||
			bytes.EqualFold(key, []byte("type")) && !bytes.Equal(key, []byte("type")) ||
			bytes.EqualFold(key, []byte("response_id")) && !bytes.Equal(key, []byte("response_id")) {
			return realtimeFrame{}, false
		}
		for _, prior := range keys[:count] {
			if bytes.Equal(prior, key) {
				return realtimeFrame{}, false
			}
		}
		keys[count], count = key, count+1
		at++
		space()
		if at >= len(data) || data[at] != ':' {
			return realtimeFrame{}, false
		}
		at++
		space()
		if at >= len(data) || data[at] == '{' || data[at] == '[' {
			return realtimeFrame{}, false
		}
		quoted := data[at] == '"'
		if quoted {
			at++
		}
		start = at
		if quoted {
			for at < len(data) && data[at] != '"' {
				if data[at] == '\\' {
					return realtimeFrame{}, false
				}
				at++
			}
		} else {
			for at < len(data) && data[at] != ',' && data[at] != '}' {
				at++
			}
		}
		if at >= len(data) {
			return realtimeFrame{}, false
		}
		value := data[start:at]
		if quoted {
			at++
		}
		switch {
		case bytes.Equal(key, []byte("type")):
			if !quoted {
				return realtimeFrame{}, false
			}
			event.Type = string(value)
		case bytes.Equal(key, []byte("response_id")):
			if !quoted {
				return realtimeFrame{}, false
			}
			event.ResponseID = string(value)
		}
		space()
		if at >= len(data) {
			return realtimeFrame{}, false
		}
		if data[at] == '}' {
			at++
			break
		}
		if data[at] != ',' {
			return realtimeFrame{}, false
		}
		at++
	}
	if at != len(data) || !json.Valid(data) {
		return realtimeFrame{}, false
	}
	return event, true
}

func realtimeMember(object oif.Value, name string) (oif.Value, bool) {
	var value oif.Value
	found := false
	for _, member := range object.Members() {
		if strings.EqualFold(member.Name, name) {
			value, found = member.Value, true
		}
	}
	return value, found
}

func realtimeStringField(object oif.Value, name string) (string, bool) {
	value, present := realtimeMember(object, name)
	if !present || value.Kind() == oif.Null {
		return "", true
	}
	if value.Kind() != oif.String {
		return "", false
	}
	raw := value.Raw()
	if !strings.ContainsRune(raw, '\\') {
		return raw[1 : len(raw)-1], true
	}
	// Escaped strings are uncommon on the hot path. JSON, rather than Go's
	// Unquote, keeps surrogate-pair interpretation identical to the old decode.
	var decoded string
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return "", false
	}
	return decoded, true
}

func realtimeIntField(object oif.Value, name string) (int64, bool) {
	value, present := realtimeMember(object, name)
	if !present || value.Kind() == oif.Null {
		return 0, true
	}
	if value.Kind() != oif.Number {
		return 0, false
	}
	number, err := strconv.ParseInt(value.Raw(), 10, 64)
	return number, err == nil
}

func (s *realtimeResponseState) clientFrame(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText || len(data) > maxRealtimeTrackedFrameBytes {
		s.unknown = true
		return
	}
	eventType := ""
	if flat, ok := realtimeFlatFrame(data); ok {
		eventType = flat.Type
	} else {
		root, valid := realtimeObject(data)
		if !valid {
			s.unknown = true
			return
		}
		eventType, valid = realtimeStringField(root, "type")
		if !valid {
			s.unknown = true
			return
		}
	}
	if eventType == "response.create" || eventType == "input_audio_buffer.commit" {
		if s.requested == maxRealtimePendingResponses {
			s.unknown = true
		} else {
			s.requested++
		}
	}
}

type realtimeFrame struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id"`
	Response   *struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			InputDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_token_details"`
		} `json:"usage"`
	} `json:"response"`
}

// decodeStrictRealtimeFrame extracts only the fields used by terminal and
// usage accounting from one duplicate-safe OIF parse. Unknown members remain
// byte-identical in the forwarded frame and are still fully syntax checked.
func decodeStrictRealtimeFrame(data []byte, event *realtimeFrame) bool {
	if flat, ok := realtimeFlatFrame(data); ok {
		*event = flat
		return true
	}
	root, valid := realtimeObject(data)
	if !valid {
		return false
	}
	if event.Type, valid = realtimeStringField(root, "type"); !valid {
		return false
	}
	if event.ResponseID, valid = realtimeStringField(root, "response_id"); !valid {
		return false
	}
	response, present := realtimeMember(root, "response")
	if !present || response.Kind() == oif.Null {
		return true
	}
	if response.Kind() != oif.Object {
		return false
	}
	event.Response = new(struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			InputDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_token_details"`
		} `json:"usage"`
	})
	if event.Response.ID, valid = realtimeStringField(response, "id"); !valid {
		return false
	}
	usageValue, present := realtimeMember(response, "usage")
	if !present || usageValue.Kind() == oif.Null {
		return true
	}
	if usageValue.Kind() != oif.Object {
		return false
	}
	event.Response.Usage = new(struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		InputDetails *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"input_token_details"`
	})
	if event.Response.Usage.InputTokens, valid = realtimeIntField(usageValue, "input_tokens"); !valid {
		return false
	}
	if event.Response.Usage.OutputTokens, valid = realtimeIntField(usageValue, "output_tokens"); !valid {
		return false
	}
	details, present := realtimeMember(usageValue, "input_token_details")
	if !present || details.Kind() == oif.Null {
		return true
	}
	if details.Kind() != oif.Object {
		return false
	}
	event.Response.Usage.InputDetails = new(struct {
		CachedTokens *int64 `json:"cached_tokens"`
	})
	cached, present := realtimeMember(details, "cached_tokens")
	if !present || cached.Kind() == oif.Null {
		return true
	}
	value, valid := realtimeIntField(details, "cached_tokens")
	if !valid {
		return false
	}
	event.Response.Usage.InputDetails.CachedTokens = &value
	return true
}

func (s *realtimeResponseState) providerFrame(typ websocket.MessageType, data []byte, strict bool) *openai.Usage {
	if typ != websocket.MessageText || len(data) > maxRealtimeTrackedFrameBytes {
		s.unknown = true
		return nil
	}
	if !strict && !bytes.Contains(data, []byte("response.done")) && bytes.IndexByte(data, '\\') < 0 {
		// Legacy sessions do not promise terminal response tracking. They
		// observe only response.done usage, and that exact type must appear
		// literally or contain a JSON escape. Other native frames still pass
		// through byte-for-byte without decoding or changed failure behavior.
		return nil
	}
	var event realtimeFrame
	valid := false
	if strict {
		valid = decodeStrictRealtimeFrame(data, &event)
	} else {
		valid = json.Unmarshal(data, &event) == nil
	}
	if !valid {
		s.unknown = true
		return nil
	}
	id := event.ResponseID
	if event.Response != nil && event.Response.ID != "" {
		id = event.Response.ID
	}
	switch event.Type {
	case "response.created":
		if s.requested > 0 {
			s.requested--
		}
		s.add(id, strict)
	case "response.done":
		if s.anonymous && len(s.active) > 0 {
			// Without a response ID on a preceding fragment, this terminal
			// cannot prove which concurrent response that fragment belonged to.
			s.unknown = true
		}
		if id != "" {
			if _, found := s.active[id]; found {
				delete(s.active, id)
			} else if s.requested > 0 {
				s.requested--
			} else if len(s.active) > 0 {
				s.unknown = true
			}
		} else if len(s.active) > 0 {
			s.unknown = true
		} else if s.requested > 0 {
			s.requested--
		}
		// Some native response fragments do not identify their response.
		// A terminal response closes that observation, while an unparseable or
		// unmatched event remains uncertain for the rest of the session.
		s.anonymous = false
	default:
		if strings.HasPrefix(event.Type, "response.") {
			if id != "" {
				if _, found := s.active[id]; !found && s.requested > 0 {
					s.requested--
				}
				s.add(id, strict)
			} else {
				s.anonymous = true
			}
		}
	}
	if event.Type != "response.done" || event.Response == nil || event.Response.Usage == nil {
		return nil
	}
	u := event.Response.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return nil
	}
	out := &openai.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputDetails != nil && u.InputDetails.CachedTokens != nil {
		cached := *u.InputDetails.CachedTokens
		if cached < 0 || cached > u.InputTokens {
			return nil
		}
		out.CachedInputTokens = &cached
	}
	return out
}

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
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
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
	if x.strict() {
		if e := strictRealtimeHandshake(snapshot, &route, r); e != nil {
			fail(e)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), realtimeSession)
	defer cancel()
	var p *pin
	if x.strict() {
		// Realtime has an operation-owned duplex contract. Retained Responses
		// eligibility and the unary codec say nothing about a native session.
		p, e = s.selectPinSurface(ctx, x, &route, "realtime", "openai", "realtime", func(provider *runtime.Provider, model string) bool {
			return provider.Connector().Supports("realtime", "openai", "realtime") && provider.Supports(model, "realtime", "openai", "realtime")
		})
	} else {
		p, e = s.selectPin(ctx, x, &route, "realtime", "realtime")
	}
	if e != nil {
		fail(e)
		return
	}
	defer s.resourceSettle(r.Context(), x, p)
	if x.strict() {
		if _, ok := snapshot.RealtimeTemplate(route.Slug, p.target.ID); !ok {
			fail(policyUnavailable("realtime_contract_unavailable", "The selected target has no compiled strict realtime contract."))
			return
		}
	}
	if e := s.reserveState(ctx, x, authority, realtimeSession); e != nil {
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
	terminalStatus := http.StatusOK
	if x.strict() {
		// A strict WebSocket has already committed HTTP 101 at this boundary.
		// Later transport errors are carried by close frames and the Attempt.
		terminalStatus = http.StatusSwitchingProtocols
	}
	conn, e := realtimeDial(ctx, s, x, p, upstream)
	if e != nil {

		client.Close(websocket.StatusInternalError, "upstream unavailable")
		x.failure = e
		if x.strict() {
			s.finish(x, &outcome{err: e, committed: true}, terminalStatus)
		} else {
			s.finish(x, nil, e.Status)
		}
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
	observedUsage, observed, relayErr := s.relayRealtime(ctx, x, p, client, conn, token, authority.ID)
	class := classProtocol
	cancelled := false
	if relayErr != nil {
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked), errors.Is(relayErr, errRealtimeProviderCredentialRevoked):
			class = classCredential
		case errors.Is(relayErr, errRealtimeClientClosed):
			class, cancelled = classCancelled, true
		case errors.Is(relayErr, context.Canceled):
			class, cancelled = classCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = classTimeout
		}
	}
	if len(x.facts) > 0 {
		fact := &x.facts[len(x.facts)-1]
		if fact.Interaction != nil {
			if observed {
				fact.Interaction.ClientState = usage.ClientPartial
			}
			if relayErr == nil {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
				fact.Interaction.ClientState = usage.ClientTerminal
			} else {
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
			}
		}
		if observedUsage != nil {
			fact.Usage = observedUsage
			fact.UsageObserved, fact.UsageComplete, fact.BillingUncertain = true, relayErr == nil, relayErr != nil
		} else {
			fact.UsageComplete = false
			fact.BillingUncertain = true
		}
		if relayErr != nil {
			fact.Class = class
		}
	}
	if relayErr != nil {
		x.failure = serverError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract.")
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked):
			x.failure = permissionError("key_revoked", "The realtime session no longer has route authority.")
		case errors.Is(relayErr, errRealtimeProviderCredentialRevoked):
			x.failure = permissionError("provider_credential_revoked", "The realtime provider credential was revoked.")
		case class == classCancelled:
			x.failure = (&attemptFailure{class: classCancelled}).toError()
		case class == classTimeout:
			x.failure = (&attemptFailure{class: classTimeout}).toError()
		}
		status := x.failure.Status
		if x.strict() {
			status = terminalStatus
		}
		s.finish(x, &outcome{err: x.failure, committed: true, cancelled: cancelled}, status)
		return
	}
	s.finish(x, &outcome{committed: true}, terminalStatus)
}

func strictRealtimeHandshake(snapshot *runtime.Snapshot, route *runtime.Route, r *http.Request) *Error {
	for _, target := range route.Targets {
		template, ok := snapshot.RealtimeTemplate(route.Slug, target.ID)
		if !ok {
			continue
		}
		if err := template.AdmitHandshake(r.URL.RawQuery, r.Header, route.Slug); err != nil {
			var refusal *realtimecontract.Refusal
			if errors.As(err, &refusal) {
				parameter := refusal.Field
				return invalidRequest("realtime_control_unavailable", refusal.Message, &parameter)
			}
			return invalidRequest("realtime_control_unavailable", "The realtime handshake is not qualified by this strict route.", nil)
		}
		return nil
	}
	return policyUnavailable("realtime_contract_unavailable", "The route has no compiled strict OpenAI realtime target.")
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
	endpoint, err := p.provider.Connector().RealtimeURL(p.model)
	if err != nil {
		return "", serverError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	return endpoint, nil
}

func realtimeDial(ctx context.Context, s *Server, x *execution, p *pin, endpoint string) (*websocket.Conn, *Error) {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	fact.Mode = "realtime"
	finish := func(class string, e *Error) *Error {
		fact.Class = class
		if fact.Interaction != nil && fact.Status > 0 {
			fact.Interaction.UpstreamState = usage.UpstreamTerminal
		}
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
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, finish(classCredential, serverError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable."))
	}
	conn, resp, err := websocket.Dial(ctx, probe.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	if err != nil {
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = usage.UpstreamUnknown
		}
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
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamAccepted
	}
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

// realtimeBoundedWriter keeps one watchdog per direction. Every frame gets a
// fresh full write window without a new context timer for each frame. The
// WebSocket's own cancellation hook still closes a stalled connection and
// interrupts a writer waiting on its internal lock.
type realtimeFrameSink interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

type realtimeBoundedWriter struct {
	dst     realtimeFrameSink
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func newRealtimeBoundedWriter(parent context.Context, dst realtimeFrameSink, timeout time.Duration) *realtimeBoundedWriter {
	w := &realtimeBoundedWriter{dst: dst, parent: parent, timeout: timeout}
	w.ctx, w.cancel = context.WithCancel(parent)
	w.timer = time.AfterFunc(timeout, func() {
		w.expired.Store(true)
		w.cancel()
	})
	w.timer.Stop()
	return w
}

func (w *realtimeBoundedWriter) close() {
	w.timer.Stop()
	w.cancel()
}

func (w *realtimeBoundedWriter) write(typ websocket.MessageType, data []byte) error {
	if err := w.parent.Err(); err != nil {
		return err
	}
	w.timer.Reset(w.timeout)
	err := w.dst.Write(w.ctx, typ, data)
	if !w.timer.Stop() {
		// The deadline won a race with successful completion. Conservatively
		// terminate this direction; it cannot start another bounded write.
		w.expired.Store(true)
		w.cancel()
	}
	if w.expired.Load() {
		return context.DeadlineExceeded
	}
	if parentErr := w.parent.Err(); parentErr != nil {
		return parentErr
	}
	return err
}

func (s *Server) relayRealtime(ctx context.Context, x *execution, p *pin, client, conn *websocket.Conn, token, keyID string) (*openai.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var usageMu sync.Mutex
	usage := &openai.Usage{}
	have := false
	observed := false
	var responses realtimeResponseState
	type relayEnd struct {
		err            error
		client         bool
		deliveryFailed bool
	}
	forward := func(dst, src *websocket.Conn, inspect bool) relayEnd {
		writer := newRealtimeBoundedWriter(ctx, dst, responseWriteTimeout)
		defer writer.close()
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				return relayEnd{err: err, client: !inspect}
			}
			if inspect {
				usageMu.Lock()
				if u := responses.providerFrame(typ, data, x.strict()); u != nil {
					usage.InputTokens = addBounded(usage.InputTokens, u.InputTokens)
					usage.OutputTokens = addBounded(usage.OutputTokens, u.OutputTokens)
					if u.CachedInputTokens != nil {
						cached := *u.CachedInputTokens
						if usage.CachedInputTokens != nil {
							cached = addBounded(cached, *usage.CachedInputTokens)
						}
						usage.CachedInputTokens = &cached
					}
					have = true
				}
				usageMu.Unlock()
			} else if x.strict() {
				usageMu.Lock()
				responses.clientFrame(typ, data)
				usageMu.Unlock()
			}
			err = writer.write(typ, data)
			if err != nil {
				return relayEnd{err: err, client: inspect, deliveryFailed: inspect}
			}
			if inspect {
				observed = true
			}
		}
	}
	done := make(chan relayEnd, 2)
	go func() { done <- forward(conn, client, false) }()
	go func() { done <- forward(client, conn, true) }()
	reauth := time.NewTicker(realtimeReauth)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	var first error
	clientClosed := false
	clientDeliveryFailed := false
	closeCode := websocket.StatusCode(0)
loop:
	for {
		select {
		case end := <-done:
			first, clientClosed, clientDeliveryFailed = end.err, end.client, end.deliveryFailed
			break loop
		case <-reauth.C:
			authority, err := s.Runtime.Authenticate(token)
			if err != nil || authority.ID != keyID || !authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) {
				first = errRealtimeAuthorityRevoked
				closeCode = websocket.StatusPolicyViolation
				client.Close(websocket.StatusPolicyViolation, "key revoked")
				break loop
			}
			if p.slot.CredentialID != nil && s.Runtime.Revoked(*p.slot.CredentialID) ||
				p.provider.Network != nil && p.provider.Network.CredentialID != "" && s.Runtime.Revoked(p.provider.Network.CredentialID) {
				first = errRealtimeProviderCredentialRevoked
				closeCode = websocket.StatusPolicyViolation
				client.Close(websocket.StatusPolicyViolation, "provider credential revoked")
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
	if closeCode != 0 {
		code = closeCode
	} else if code < 0 {
		code = websocket.StatusInternalError
	}
	usageMu.Lock()
	pending := x.strict() && responses.pending()
	usageMu.Unlock()
	if !clientClosed && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		// The provider closed a socket while a native response still owed
		// its response.done terminal event. Never mirror its 1000/1001 as
		// a successful client-visible session end.
		code = websocket.StatusInternalError
	}
	conn.Close(code, "")
	client.Close(code, "")
	second := <-done
	clientDeliveryFailed = clientDeliveryFailed || second.deliveryFailed
	usageMu.Lock()
	defer usageMu.Unlock()
	var observedUsage *openai.Usage
	if have {
		observedUsage = usage
	}
	pending = x.strict() && responses.pending()
	if clientClosed && (pending || clientDeliveryFailed) {
		return observedUsage, observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, first)
	}
	if !clientClosed && pending && (websocket.CloseStatus(first) == websocket.StatusNormalClosure || websocket.CloseStatus(first) == websocket.StatusGoingAway) {
		return observedUsage, observed, fmt.Errorf("%w: %v", errRealtimeResponseIncomplete, first)
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return observedUsage, observed, nil
	}
	if clientClosed {
		return observedUsage, observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, first)
	}
	if !have {
		return nil, observed, first
	}
	return usage, observed, first
}

func strPtr(value string) *string { return &value }
