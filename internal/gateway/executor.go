package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Failure classes shared with the routing retry taxonomy fixture.
const (
	classSuccess        = "success"
	classConnect        = "connect"
	classTimeout        = "timeout"
	classRateLimit      = "rate_limit"
	classUpstreamServer = "upstream_server"
	classUpstreamClient = "upstream_client"
	classCredential     = "credential"
	classProtocol       = "protocol"
	classCancelled      = "cancelled"
)

// The only operation and surface this gateway serves.
const (
	operationGeneration = "generation"
	surfaceOpenAI       = "openai"
)

// failoverAllowed reports whether a failure class may select another
// attempt when nothing has been committed to the client.
func failoverAllowed(class string, committed bool) bool {
	if committed {
		return false
	}
	switch class {
	case classConnect, classTimeout, classRateLimit, classUpstreamServer, classCredential:
		return true
	}
	return false
}

const (
	maxStreamDuration = time.Hour
	errorBodyLimit    = 64 * 1024
)

// execution is one inference request flowing through the attempt loop.
type execution struct {
	request  request
	family   openai.Family
	parsed   *openai.Request
	actor    string
	keyID    string
	userID   string
	affinity []byte
	route    *runtime.Route
	mode     string
	attempts []runtime.Attempt
	budget   int
	emit     openai.Emit // streaming only
	estimate int64       // per-attempt token estimate used for admission and settlement

	once       sync.Once
	facts      []AttemptFact
	failure    *Error         // terminal error decided before the attempt loop ran
	firstByte  *time.Duration // request start to the first payload byte the client received
	lease      *limits.Lease  // the API key reservation, settled once the request ends
	dispatched bool           // at least one attempt was handed to a provider
}

// usage is the metering evidence the request ends with: the usage of the
// attempt that served it, which is the last one recorded.
func (x *execution) usage() *openai.Usage {
	if len(x.facts) == 0 {
		return nil
	}
	return x.facts[len(x.facts)-1].Usage
}

// delivered records when the first byte of the response payload reached the
// client. Later deliveries keep the first one.
func (x *execution) delivered(at time.Time) {
	if x.firstByte == nil {
		elapsed := at.Sub(x.request.startedAt)
		x.firstByte = &elapsed
	}
}

// outcome is the terminal result of the attempt loop.
type outcome struct {
	completion *openai.Completion
	err        *Error
	committed  bool
	cancelled  bool
}

// dispatchableAttempts returns the most attempts this request can hand to a
// provider, capped by its requested budget. Each target may be tried through
// each of its usable credential slots.
func (s *Server) dispatchableAttempts(x *execution) int {
	remaining := x.budget
	available := 0
	for _, attempt := range x.attempts {
		provider, ok := x.request.release.Snapshot.Providers[attempt.ProviderID]
		if !ok {
			continue
		}
		for i := range provider.Slots {
			if s.slotAvailable(x, attempt, &provider.Slots[i]) {
				available++
			}
			if available == remaining {
				return available
			}
		}
	}
	return available
}

type attemptFailure struct {
	class      string
	status     int
	committed  bool
	retryAfter time.Duration
	upstream   *openai.UpstreamError
	overall    bool   // the route deadline, not the attempt deadline, expired
	dispatched bool   // the request reached the upstream before the failure
	quota      string // a quota this gateway enforces rejected the attempt
}

// The quotas that can reject an attempt before it is dispatched.
const (
	quotaConnection = "connection"
	quotaSlot       = "slot"
)

// billingUncertain reports whether the upstream may have served and billed
// work this attempt cannot account for. Anything already delivered to the
// client was served, and a request that reached the upstream may have been
// processed in full even though its result never came back. A rejection the
// upstream stated — a bad request, an exhausted quota, a refused credential —
// costs nothing, and neither does a failure that never left this gateway.
// The phase, not the class, decides: classConnect covers every transport
// failure here, including a connection lost long after the request was sent.
func (f *attemptFailure) billingUncertain() bool {
	if f.committed {
		return true
	}
	switch f.class {
	case classRateLimit, classUpstreamClient, classCredential:
		return false
	}
	return f.dispatched
}

func (f *attemptFailure) toError() *Error {
	switch f.class {
	case classCancelled:
		return &Error{Status: 0, Code: "client_cancelled", Message: "The client went away."}
	case classTimeout:
		return serverError(http.StatusGatewayTimeout, "gateway_timeout", "The upstream provider did not respond within the configured deadline.")
	case classConnect:
		return serverError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider could not be reached.")
	case classRateLimit:
		// A quota this gateway enforces was never the upstream's decision, and
		// saying so would send the caller looking at the wrong system.
		switch f.quota {
		case quotaConnection:
			return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider connection limit was exceeded.", RetryAfter: f.retryAfter}
		case quotaSlot:
			return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "The provider credential limit was exceeded.", RetryAfter: f.retryAfter}
		}
		return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "upstream_rate_limit", Message: "The upstream provider is rate limiting requests.", RetryAfter: f.retryAfter}
	case classUpstreamServer:
		return serverError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider failed with HTTP "+strconv.Itoa(f.status)+".")
	case classCredential:
		code := "upstream_authentication_failed"
		if f.status == http.StatusForbidden {
			code = "upstream_permission_denied"
		}
		return serverError(http.StatusBadGateway, code, "The upstream provider rejected the configured credential.")
	case classProtocol:
		return serverError(http.StatusBadGateway, "provider_protocol_error", "The upstream provider returned a malformed response.")
	case classUpstreamClient:
		message := "The upstream provider rejected the request."
		if f.upstream != nil && f.upstream.Message != "" {
			message = f.upstream.Message
		}
		if forwardable(f.status) {
			return &Error{Status: f.status, Type: "invalid_request_error", Code: "upstream_rejected", Message: message}
		}
		return serverError(http.StatusBadGateway, "upstream_rejected", message)
	}
	return serverError(http.StatusBadGateway, "upstream_unavailable", "The upstream provider failed.")
}

// forwardable reports whether an upstream client-error status is returned to
// the caller unchanged; every other upstream rejection is a gateway failure.
func forwardable(status int) bool {
	switch status {
	case 400, 404, 405, 409, 413, 415, 422:
		return true
	}
	return false
}

// execute runs the bounded attempt loop against the pinned release.
func (s *Server) execute(ctx context.Context, x *execution) *outcome {
	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	deadline, _ := ctx.Deadline()
	snapshot := x.request.release.Snapshot
	used := 0
	unmeterable := false
	var last *attemptFailure
	for _, attempt := range x.attempts {
		if used >= x.budget || ctx.Err() != nil {
			break
		}
		provider, ok := snapshot.Providers[attempt.ProviderID]
		if !ok || s.health.open(provider.ID) {
			continue
		}
		slots := s.slots(x, attempt, &provider)
		if len(slots) == 0 {
			continue
		}
		next := false
		for _, slot := range slots {
			if used >= x.budget || ctx.Err() != nil {
				break
			}
			// Authority can change while an earlier credential attempt is pending.
			if provider.AuthMode != "none" && s.Runtime.Revoked(*slot.CredentialID) {
				continue
			}
			// attempt.Timeout bounds first-byte/idle waits, not the lifetime
			// of a stream. Hold concurrency through the overall deadline.
			timeout := time.Until(deadline)
			if timeout <= 0 {
				// The route deadline is spent, so there is no window left to
				// reserve and nothing further to try.
				return &outcome{err: (&attemptFailure{class: classTimeout}).toError()}
			}
			// A cooldown another replica recorded is read here rather than while
			// the slots are ranked, so a limiter answering slowly costs one round
			// trip per slot actually tried instead of one per candidate slot.
			if s.Admission.cooling(ctx, provider.ID, &slot) {
				continue
			}
			estimate := estimateTokens(x.parsed, provider.ParameterDefaults)
			reservation, rejection, skip := s.Admission.reserveTarget(ctx, &provider, &slot, estimate, timeout)
			if skip {
				unmeterable = true
				continue
			}
			if rejection != nil {
				// The quota rejected the attempt before the provider was
				// called, so it cost the upstream nothing and a sibling
				// target may still serve this request.
				used++
				x.facts = append(x.facts, s.rejectedFact(x, attempt, slot, used, rejection))
				last = rejection
				if rejection.quota == quotaConnection {
					break // every credential shares the connection quota
				}
				continue
			}
			used++
			fact, completion, failure := s.attempt(ctx, x, attempt, &provider, slot, used)
			// Only an attempt that reached the upstream spent the key's window.
			// An attempt that died inside this gateway — an endpoint outside the
			// egress policy, a body that would not encode, a credential that
			// would not apply — cost no provider anything, so the request stays
			// refundable.
			dispatched := failure == nil || failure.dispatched
			x.dispatched = x.dispatched || dispatched
			reservation.settle(ctx, dispatched, totalTokens(fact.Usage))
			x.facts = append(x.facts, fact)
			s.health.record(provider.ID, fact)
			if failure == nil {
				return &outcome{completion: completion, committed: fact.Committed}
			}
			if failure.overall || failure.class == classCancelled {
				return &outcome{err: failure.toError(), committed: failure.committed, cancelled: failure.class == classCancelled}
			}
			if !failoverAllowed(failure.class, failure.committed) {
				return &outcome{err: failure.toError(), committed: failure.committed}
			}
			last = failure
			switch failure.class {
			case classCredential:
				s.health.cooldown(provider.ID, slot.ID, credentialCooldown)
				s.Admission.cooldown(ctx, provider.ID, &slot, credentialCooldown)
			case classRateLimit:
				s.health.cooldown(provider.ID, slot.ID, failure.retryAfter)
				s.Admission.cooldown(ctx, provider.ID, &slot, cooldownDuration(failure.retryAfter))
			default:
				next = true // the provider itself failed; sibling credentials would too
			}
			if next {
				break
			}
		}
	}
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return &outcome{err: (&attemptFailure{class: classTimeout}).toError()}
	case ctx.Err() != nil:
		return &outcome{err: (&attemptFailure{class: classCancelled}).toError(), cancelled: true}
	case last != nil:
		return &outcome{err: last.toError()}
	case unmeterable:
		// Every eligible target had a quota that could not be consulted.
		// Serving unmetered would spend a budget nobody can account for.
		return &outcome{err: limitsUnavailable()}
	}
	return &outcome{err: serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.route.Slug+"`.")}
}

// slots returns the credential slots usable for this attempt, ordered by
// priority then by deterministic weighted rendezvous on the request's
// affinity so sibling keys spread across a pool.
func (s *Server) slots(x *execution, attempt runtime.Attempt, provider *runtime.Provider) []runtime.Slot {
	type ranked struct {
		slot  runtime.Slot
		score float64
	}
	var usable []ranked
	for _, slot := range provider.Slots {
		if !s.slotAvailable(x, attempt, &slot) || s.health.coolingDown(provider.ID, slot.ID) {
			continue
		}
		score := 0.0
		if routeID, err := uuid.Parse(x.route.ID); err == nil {
			if slotID, err := uuid.Parse(slot.ID); err == nil {
				score = runtime.Score(routeID, slotID, slot.Weight, operationGeneration, surfaceOpenAI, x.mode, x.affinity)
			}
		}
		usable = append(usable, ranked{slot, score})
	}
	sort.SliceStable(usable, func(i, j int) bool {
		if usable[i].slot.Priority != usable[j].slot.Priority {
			return usable[i].slot.Priority < usable[j].slot.Priority
		}
		return usable[i].score > usable[j].score
	})
	out := make([]runtime.Slot, 0, len(usable))
	for _, r := range usable {
		out = append(out, r.slot)
	}
	return out
}

func (s *Server) slotAvailable(x *execution, attempt runtime.Attempt, slot *runtime.Slot) bool {
	if !slot.Allows(attempt.UpstreamModel, x.route.Slug, x.keyID) {
		return false
	}
	provider := x.request.release.Snapshot.Providers[attempt.ProviderID]
	if provider.AuthMode == "none" {
		return true
	}
	if slot.CredentialID == nil || s.Runtime.Revoked(*slot.CredentialID) {
		return false
	}
	_, ok := x.request.release.Credential(*slot.CredentialID)
	return ok
}

// attemptState tracks why an attempt context ended.
type attemptState struct {
	parent     context.Context
	reason     atomic.Int32 // 1 first-byte deadline, 2 idle deadline, 3 stream cap
	dispatched atomic.Bool
}

// trace conservatively marks dispatch once writing has begun or a response
// arrives. A body write failure does not prove that the upstream saw nothing;
// only a failure before writing is safely refundable.
func (st *attemptState) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		// A failed body write can still leave work at the upstream.
		WroteHeaders: func() { st.dispatched.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				st.dispatched.Store(true)
			}
		},
		GotFirstResponseByte: func() { st.dispatched.Store(true) },
	}
}

func (st *attemptState) classify(err error, committed bool) string {
	switch {
	case st.parent.Err() != nil:
		if errors.Is(st.parent.Err(), context.DeadlineExceeded) {
			return classTimeout
		}
		return classCancelled
	case st.reason.Load() != 0:
		return classTimeout
	case errors.Is(err, errClientWrite):
		return classCancelled
	case committed:
		return classProtocol
	}
	var pe *openai.ProtocolError
	if errors.As(err, &pe) || errors.Is(err, openai.ErrEventTooLarge) {
		return classProtocol
	}
	return classConnect
}

func endpointPath(family openai.Family) string {
	if family == openai.FamilyResponses {
		return "/responses"
	}
	return "/chat/completions"
}

// newFact opens the record of one attempt against one credential slot.
func (s *Server) newFact(x *execution, a runtime.Attempt, slot runtime.Slot, ordinal int) AttemptFact {
	fact := AttemptFact{
		Ordinal:            ordinal,
		TargetID:           a.TargetID,
		ProviderID:         a.ProviderID,
		ProviderRevisionID: a.ProviderRevisionID,
		UpstreamModel:      a.UpstreamModel,
		SlotID:             slot.ID,
		Mode:               x.mode,
		StartedAt:          s.now(),
	}
	if slot.CredentialID != nil {
		fact.CredentialID = *slot.CredentialID
	}
	if slot.CredentialVersion != nil {
		fact.CredentialVersion = *slot.CredentialVersion
	}
	return fact
}

// rejectedFact records an attempt a quota refused. Nothing was sent, so the
// attempt carries no status and cannot have been billed by anyone.
func (s *Server) rejectedFact(x *execution, a runtime.Attempt, slot runtime.Slot, ordinal int, rejection *attemptFailure) AttemptFact {
	fact := s.newFact(x, a, slot, ordinal)
	fact.Class = rejection.class
	fact.Duration = s.now().Sub(fact.StartedAt)
	if rejection.retryAfter > 0 {
		retry := rejection.retryAfter
		fact.RetryAfter = &retry
	}
	fact.recordEvidence(false)
	return fact
}

// attempt performs one upstream call with one credential.
func (s *Server) attempt(ctx context.Context, x *execution, a runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, *openai.Completion, *attemptFailure) {
	fact := s.newFact(x, a, slot, ordinal)
	st := &attemptState{parent: ctx}
	fail := func(class string, f *attemptFailure) (AttemptFact, *openai.Completion, *attemptFailure) {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		f.dispatched = st.dispatched.Load()
		fact.Class = class
		fact.Committed = f.committed
		fact.Duration = s.now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			retry := f.retryAfter
			fact.RetryAfter = &retry
		}
		fact.recordEvidence(f.billingUncertain())
		return fact, nil, f
	}

	endpoint, err := s.egress.ValidateEndpoint(provider.Endpoint)
	if err != nil {
		return fail(classConnect, nil)
	}
	body, err := x.parsed.Encode(a.UpstreamModel, provider.ParameterDefaults)
	if err != nil {
		return fail(classProtocol, nil)
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(classTimeout, &attemptFailure{overall: true})
	}
	timeout := min(a.Timeout, remaining)

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	firstByte := time.AfterFunc(timeout, func() { st.reason.CompareAndSwap(0, 1); cancel() })
	defer firstByte.Stop()

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, st.trace()), http.MethodPost, endpoint.String()+endpointPath(x.family), bytes.NewReader(body))
	if err != nil {
		return fail(classConnect, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "olp-go/gateway")
	req.Header.Set("Accept", "application/json")
	if x.parsed.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	var credentialValues []string
	if slot.CredentialID != nil {
		secret, _ := x.request.release.Credential(*slot.CredentialID)
		switch provider.AuthMode {
		case "headers":
			if err := egress.ApplyCredentialHeaders(req.Header, provider.CredentialHeaders, secret); err != nil {
				return fail(classCredential, nil)
			}
			credentialValues = append(credentialValues, string(secret))
			for _, name := range provider.CredentialHeaders {
				credentialValues = append(credentialValues, req.Header.Values(name)...)
			}
		case "none":
		default:
			req.Header.Set("Authorization", "Bearer "+string(secret))
			credentialValues = append(credentialValues, string(secret))
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fail(st.classify(err, false), nil)
	}
	defer resp.Body.Close()
	// The response status is the first thing the upstream sends back.
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		f := &attemptFailure{status: resp.StatusCode, upstream: openai.ParseErrorBody(raw)}
		if f.upstream != nil {
			f.upstream.Message = redactCredentials(f.upstream.Message, credentialValues)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fail(classCredential, f)
		case resp.StatusCode == http.StatusTooManyRequests:
			f.retryAfter = retryAfter(resp.Header.Get("Retry-After"), s.now())
			return fail(classRateLimit, f)
		case resp.StatusCode >= 500:
			return fail(classUpstreamServer, f)
		}
		return fail(classUpstreamClient, f)
	}

	var completion *openai.Completion
	committed := false
	if x.parsed.Stream {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaType != "text/event-stream" {
			return fail(classProtocol, &attemptFailure{status: resp.StatusCode})
		}
		streamCap := time.AfterFunc(maxStreamDuration, func() { st.reason.CompareAndSwap(0, 3); cancel() })
		defer streamCap.Stop()
		var watchdog *time.Timer
		defer func() {
			if watchdog != nil {
				watchdog.Stop()
			}
		}()
		emit := func(frame []byte) error {
			if !committed {
				committed = true
				firstByte.Stop()
				watchdog = time.AfterFunc(a.Timeout, func() { st.reason.CompareAndSwap(0, 2); cancel() })
			} else {
				watchdog.Reset(a.Timeout)
			}
			return x.emit(frame)
		}
		completion, err = openai.StreamMetadata(x.family, resp.Body, int(s.cfg.MaxEventBytes), x.route.Slug, x.parsed.IncludeUsage, emit)
	} else {
		limited := &countingReader{r: resp.Body, limit: s.cfg.MaxResponseBytes}
		raw, readErr := io.ReadAll(limited)
		if readErr != nil {
			err = readErr
		} else if x.family == openai.FamilyResponses {
			completion, err = openai.DecodeResponse(raw, x.route.Slug)
		} else {
			completion, err = openai.DecodeChat(raw, x.route.Slug)
		}
	}
	if completion != nil {
		fact.Usage = completion.Usage
	}
	if err != nil {
		f := &attemptFailure{status: resp.StatusCode, committed: committed}
		var ue *openai.UpstreamError
		switch {
		case errors.Is(err, errResponseTooLarge):
			return fail(classProtocol, f)
		case errors.As(err, &ue):
			f.upstream = ue
			if committed {
				return fail(classProtocol, f)
			}
			return fail(classUpstreamServer, f)
		}
		return fail(st.classify(err, committed), f)
	}
	fact.Class = classSuccess
	fact.Committed = committed
	fact.Duration = s.now().Sub(fact.StartedAt)
	// A success carrying no usage was still served and billed upstream, with
	// nothing this gateway can meter.
	fact.recordEvidence(true)
	return fact, completion, nil
}

// countingReader enforces the buffered unary response byte limit.
type countingReader struct {
	r        io.Reader
	limit    int64
	read     int64
	exceeded bool
}

var errResponseTooLarge = errors.New("upstream response exceeds byte limit")

func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.exceeded {
		return 0, errResponseTooLarge
	}
	if c.read == c.limit {
		// Reaching the limit is valid if the upstream ends here. Read at most
		// one additional byte to distinguish EOF from an oversized response.
		var extra [1]byte
		n, err := c.r.Read(extra[:])
		if n > 0 {
			c.exceeded = true
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > c.limit-c.read {
		p = p[:c.limit-c.read]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// retryAfter parses a Retry-After header as seconds or an HTTP date.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		for _, c := range value {
			if c < '0' || c > '9' {
				return 0
			}
		}
		const maxSeconds = uint64((1<<63 - 1) / time.Second)
		return time.Duration(min(seconds, maxSeconds)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// finish emits the terminal envelope exactly once.
func (s *Server) finish(x *execution, out *outcome, status int) {
	x.once.Do(func() {
		completedAt := s.now()
		env := Envelope{
			RequestID:    x.request.id,
			AccountingID: x.request.accountingID(),
			ClientIP:     x.request.clientIP,
			Actor:        x.actor,
			KeyID:        x.keyID,
			UserID:       x.userID,
			Family:       string(x.family),
			Mode:         x.mode,
			Operation:    operationGeneration,
			Surface:      surfaceOpenAI,
			Outcome:      "failure",
			Status:       status,
			StartedAt:    x.request.startedAt,
			CompletedAt:  completedAt,
			Duration:     completedAt.Sub(x.request.startedAt),
			FirstByte:    x.firstByte,
			Attempts:     x.facts,
		}
		if x.request.release != nil {
			env.ReleaseSequence = x.request.release.Sequence
			if x.request.release.Snapshot != nil {
				env.RuntimeGenerationID = x.request.release.Snapshot.Generation.ID
			}
		}
		if x.route != nil {
			env.Route = x.route.Slug
			env.RouteRevisionID = x.route.RevisionID
		}
		env.Usage = x.usage()
		switch {
		case out != nil && out.err != nil:
			env.ErrorClass = out.err.Code
		case out == nil && x.failure != nil:
			env.ErrorClass = x.failure.Code
		}
		if out != nil {
			env.Committed = out.committed
			switch {
			case out.cancelled:
				env.Outcome = "cancelled"
				env.Status = 0
			case out.err == nil:
				env.Outcome = "success"
			}
		}
		s.Sink.Terminal(env)
	})
}
