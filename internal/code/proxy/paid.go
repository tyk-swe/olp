package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/session"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	protocolwire "github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type paidAttempt struct {
	core          attemptCore
	config        routing.Configuration
	auth          code.Authorization
	sensitive     egress.Sensitive
	provider      routing.Provider
	slot          routing.Slot
	connector     connectors.Config
	family        protocolwire.Format
	route         code.Route
	release       *release.Release
	authority     keyauthority.Authority
	lease         *admission.Lease
	targetLease   PaidTargetLease
	supplyLeases  []*admission.Lease
	price         *usage.RoutingPrice
	discount      *usage.RoutingDiscount
	card          *usage.RoutingCardRevision
	started       time.Time
	measured      *protocolwire.Usage
	usageComplete bool
	stream        bool
}

func protocolFamily(protocol code.Protocol) protocolwire.Format {
	switch protocol {
	case code.ProtocolResponses:
		return protocolwire.Responses
	case code.ProtocolMessages:
		return protocolwire.Anthropic
	default:
		return protocolwire.Chat
	}
}

func (s *Server) preparePaid(
	r *http.Request,
	rel *release.Release,
	route code.Route,
	observation code.Request,
	operation code.Operation,
	ingress vendor.Endpoint,
	transport code.Transport,
	verified keyauthority.Authority,
	body []byte,
	recovery *code.Recovery,
	fallback bool,
) (*paidAttempt, error) {
	if !route.PaidParticipation {
		return nil, code.Refuse(403, "code_paid_participation_disabled")
	}
	if transport != code.HTTP || operation.Name == "compact" || operation.Name == "prewarm" {
		return nil, code.Refuse(400, "code_paid_operation_unsupported")
	}
	target := route.PaidTarget(operation.Model, fallback)
	provider, exists := rel.Snapshot().CodePaidConnection(route, target.ProviderID)
	slotIndex := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
	if !exists || !provider.Enabled || provider.ProjectID == nil || *provider.ProjectID != route.ProjectID || slotIndex < 0 {
		return nil, code.Refuse(503, "code_paid_target_unavailable")
	}
	slot := provider.Slots[slotIndex]
	if !slot.Enabled || slot.CredentialID == nil || slot.CredentialVersion == nil || !slot.Allows(operation.Model, route.Slug, verified.ID) || !s.Host.IsCredentialEligible(*slot.CredentialID) {
		return nil, code.Refuse(403, "code_paid_target_denied")
	}
	connector := provider.Connector()
	family := protocolFamily(ingress.Protocol)
	native, _, nativeOK := routing.CodePaidProtocol(provider, operation.Model)
	if !nativeOK || native != ingress.Protocol {
		return nil, code.Refuse(400, "code_paid_operation_unsupported")
	}
	parsed, parseErr := protocols.ParseForModel(family, body, operation.PublicModel())
	if parseErr != nil {
		return nil, code.Refuse(400, "code_body_invalid")
	}
	mode := "unary"
	if parsed.Stream {
		mode = "streaming"
	}
	if !provider.Supports(operation.Model, "generation", family.Surface(), mode) {
		return nil, code.Refuse(400, "code_paid_operation_unsupported")
	}
	demand := estimate.Walk(parsed).Estimate(estimate.ForModel(operation.Model), nil)
	now := s.Host.Now()
	inputs := s.Host.RoutingInputs()
	price := inputs.Price(provider.Kind, provider.ID, provider.VendorID, operation.Model, "generation", now)
	if price == nil {
		return nil, code.Refuse(503, "code_paid_pricing_unavailable")
	}
	discount := inputs.Discount(provider.Kind, provider.ID, provider.VendorID, now)
	cost, priced := price.CostBound(demand.InputExposure(), demand.Reply(), true, now, discount)
	if !priced {
		return nil, code.Refuse(503, "code_paid_pricing_unavailable")
	}
	card, cardKnown := inputs.RateCardRevision(&route.ProjectID, now)
	if !cardKnown {
		return nil, code.Refuse(503, "code_paid_pricing_unavailable")
	}
	charge, chargePriced := cost, true
	if card != nil {
		charge, chargePriced = card.ChargeBound(demand.InputExposure(), demand.Reply(), true, now, cost, provider.Kind, provider.VendorID, operation.Model, "generation")
	}
	if !chargePriced {
		return nil, code.Refuse(503, "code_paid_pricing_unavailable")
	}
	labels, failStatus, failCode, ok := s.Host.ParseAttribution(r, verified)
	if !ok {
		return nil, code.Refuse(failStatus, "code_"+failCode)
	}
	providersFor := func(model string) []string {
		return rel.Snapshot().CodeModelProviders(route, ingress.ClientPath, model, transport)
	}
	permit, err := s.Ledger.AdmitPaid(r.Context(), session.PaidAdmission{
		Admission: session.Admission{
			Route: route, APIKeyID: verified.ID, EndUserDigest: verified.EndUserDigest,
			Attribution: labels, Operation: operation, PreviousResponse: observation.PreviousResponse,
			InitialRecovery: observation.Recoverable, Recovery: recovery, ProvidersFor: providersFor,
		},
		Fallback: fallback, Authority: verified, Provider: provider, Slot: slot,
	})
	if err != nil {
		return nil, err
	}
	cfg := routing.Configuration{Identity: providerconfig.Identity[string]{Kind: provider.Kind, Endpoint: provider.Endpoint}}
	cfg.Options.Network = provider.Network
	p := &paidAttempt{
		core: attemptCore{
			server: s, attemptID: permit.Attempt.ID, model: operation.Model,
			publicModel: operation.PublicModel(), protocol: ingress.Protocol,
			observer: wire.NewObserver(vendor.Endpoint{Protocol: ingress.Protocol}),
		},
		config: cfg, provider: provider, slot: slot, connector: connector,
		family: family, route: route, release: rel, authority: permit.Authority,
		price: price, discount: discount, card: card, started: now, stream: parsed.Stream,
	}
	ready := false
	defer func() {
		if !ready {
			p.finish(r.Context())
		}
	}()
	if err := BindWorkload(&p.authority, verified); err != nil {
		return nil, err
	}
	if failStatus, allowed := s.Host.CheckKeyAddress(r, p.authority); !allowed {
		return nil, code.Refuse(failStatus, "code_ip_not_allowed")
	}
	fresh, _, validEndUser := s.Host.IdentifyEndUser(r, p.authority)
	if !validEndUser || fresh.EndUserDigest != verified.EndUserDigest {
		return nil, code.Refuse(403, "code_end_user_invalid")
	}
	p.authority = fresh
	p.authority.Attribution = labels
	lease, targetLease, supplyLeases, err := s.Host.ReservePaid(r.Context(), &p.authority, route, provider, slot, demand.Tokens(), GenerationTimeout+time.Minute, permit.Attempt.ID, cost.String(), charge.String())
	p.lease, p.targetLease, p.supplyLeases = lease, targetLease, supplyLeases
	if err != nil {
		return nil, err
	}
	ready = true
	return p, nil
}

func (p *paidAttempt) serveHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request, rel *release.Release, raw, body []byte, refuse func(error)) {
	payload := raw
	if p.core.model != p.core.publicModel {
		var err error
		payload, err = wire.RewriteRequestModel(body, p.core.publicModel, p.core.model)
		if err != nil {
			refuse(err)
			return
		}
	}
	httpClient, err := p.core.server.Host.ConnectionClient(ctx, rel, p.config, p.provider.ID)
	if err != nil {
		refuse(err)
		return
	}
	defer httpClient.CloseIdleConnections()
	target, err := p.requestURL(r.URL.RawQuery)
	if err != nil {
		refuse(err)
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &oneShotBody{data: payload})
	if err != nil {
		refuse(code.Refuse(502, "code_connection_invalid"))
		return
	}
	request.ContentLength = int64(len(payload))
	request.Header = buildUpstreamHeaders(r.Header, nil, "")
	if p.core.model != p.core.publicModel {
		request.Header.Del("Content-Encoding")
		request.Header.Del("Content-Length")
		request.Header.Set("Accept-Encoding", "identity")
	}
	request.Trailer = wire.ForwardTrailers(r.Trailer, r.Header, true)
	if len(request.Trailer) != 0 {
		request.ContentLength = -1
		request.Header.Del("Content-Length")
	}
	request.GetBody = nil
	request.Close = true
	if err := p.authorize(request, payload); err != nil {
		refuse(err)
		return
	}
	if err := p.core.dispatch(ctx); err != nil {
		refuse(err)
		return
	}
	upstreamResponse, err := httpClient.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			p.core.recordOutcome(ctx, code.GatewayOutcome(code.OutcomeTransportError, p.core.server.Host.Now().UTC()))
		}
		refuse(code.Refuse(502, "code_upstream_unavailable"))
		return
	}
	defer upstreamResponse.Body.Close()
	p.onResponseHeaders(ctx, upstreamResponse.StatusCode)
	response, safetyErr := p.safeResponse(upstreamResponse)
	if safetyErr != nil {
		refuse(safetyErr)
		return
	}
	defer response.Body.Close()
	writeUpstreamResponseHeaders(w, response, p.core.model != p.core.publicModel)
	meteredResponse, finishUsage := p.meterResponse(response)
	defer finishUsage()
	copyHTTPResponse(ctx, w, meteredResponse, &p.core, p.observe, p.observeUnary, func(ctx context.Context, _ error) {
		if ctx.Err() == nil {
			p.core.recordOutcome(ctx, code.GatewayOutcome(code.OutcomeTransportError, p.core.server.Host.Now().UTC()))
		}
	})
	writeUpstreamResponseTrailers(w, response)
}

func (p *paidAttempt) requestURL(query string) (string, error) {
	target, err := p.connector.URL(p.family, p.core.model, p.stream)
	if err != nil {
		return "", code.Refuse(502, "code_connection_invalid")
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", code.Refuse(502, "code_connection_invalid")
	}
	if err := p.core.server.Host.ValidateEndpoint(u.String()); err != nil {
		return "", code.Refuse(502, "code_egress_refused")
	}
	if query != "" {
		if u.RawQuery != "" {
			return "", code.Refuse(400, "code_query_unsupported")
		}
		u.RawQuery = query
	}
	return u.String(), nil
}

func (p *paidAttempt) authorize(request *http.Request, body []byte) error {
	secret, generation, err := p.core.server.Host.Secret(request.Context(), p.release, *p.slot.CredentialID)
	if err != nil {
		return code.Refuse(503, "code_paid_credential_unavailable")
	}
	if err := p.connector.ApplySemantic(request); err != nil {
		return code.Refuse(503, "code_paid_configuration_unsupported")
	}
	sensitive, err := p.core.server.Host.ApplyConnectorAuth(request.Context(), request, p.connector, secret, body)
	if err != nil {
		return code.Refuse(503, "code_paid_credential_unavailable")
	}
	p.sensitive = sensitive
	p.auth = code.Authorization{CredentialID: *p.slot.CredentialID, GrantGeneration: generation}
	return nil
}

func (p *paidAttempt) onResponseHeaders(ctx context.Context, status int) {
	p.core.status = status
	p.core.recordOutcome(ctx, code.UpstreamHeadersOutcome(status, p.core.server.Host.Now().UTC()))
	if status == http.StatusUnauthorized {
		p.core.server.Host.CredentialRefused(p.auth.CredentialID, p.auth.GrantGeneration)
	}
}

func (p *paidAttempt) observeUnary(body []byte) {
	observation := p.core.observer.Unary(body)
	p.observe(observation)
	if p.core.status >= 400 {
		p.core.terminal = true
	}
}

func (p *paidAttempt) observe(o code.Observation) {
	p.core.recordReference(o.ResponseID)
	if o.Status != 0 {
		p.onResponseHeaders(context.Background(), o.Status)
	}
	if o.Outcome != nil {
		p.core.recordOutcome(context.Background(), *o.Outcome)
	}
	p.core.applyTerminal(o)
}

func (p *paidAttempt) finish(ctx context.Context) {
	if p.core.finished {
		return
	}
	p.core.finished = true
	p.core.settleAttempt(ctx)
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	observed := p.measured != nil && p.usageComplete && p.core.terminal && !p.core.conflicting
	costText, chargeText := "", ""
	if p.core.dispatched && p.measured != nil {
		cost, known := p.price.Cost(usage.ObservedAttemptUsage(p.measured, observed, "generation"), p.started, p.core.status >= 200 && p.core.status < 300, p.discount)
		if known {
			costText = cost.String()
			if p.card == nil {
				chargeText = costText
			} else if charge, chargeKnown := p.card.Charge(usage.ObservedAttemptUsage(p.measured, observed, "generation"), p.started, p.core.status >= 200 && p.core.status < 300, &cost, p.provider.Kind, p.provider.VendorID, p.core.model, "generation"); chargeKnown {
				chargeText = charge.String()
			}
		}
	}
	if p.lease != nil {
		p.lease.SetActualCost(costText, chargeText)
	}
	for _, lease := range p.supplyLeases {
		lease.SetActualCost(costText, costText)
	}
	if p.targetLease != nil {
		p.targetLease.Settle(step, p.core.dispatched, p.core.usage.Total)
	}
	for _, lease := range p.supplyLeases {
		p.core.server.Host.SettleLease(step, lease, p.core.dispatched, p.core.usage.Total)
	}
	p.core.server.Host.SettleLease(step, p.lease, p.core.dispatched, p.core.usage.Total)
	if !p.core.dispatched {
		return
	}
	p.core.server.Host.RecordPaidTerminal(PaidTerminal{
		AttemptID: p.core.attemptID, Authority: p.authority, Route: p.route,
		RuntimeGenerationID: p.release.Snapshot().Generation().ID, ReleaseSequence: p.release.Sequence,
		Provider: p.provider, Slot: p.slot, Model: p.core.model, Family: p.family,
		Stream: p.stream, Status: p.core.status, StartedAt: p.started, CompletedAt: p.core.server.Host.Now(),
		Usage: p.measured, UsageObserved: p.measured != nil, UsageComplete: observed,
		BillingUncertain: p.core.dispatched && !observed, Price: p.price, Discount: p.discount, Card: p.card,
	})
}

type paidUsageObservation struct {
	usage    *protocolwire.Usage
	complete bool
}

type paidUsageWriter struct{ writer *io.PipeWriter }

func (w paidUsageWriter) Write(data []byte) (int, error) {
	_, _ = w.writer.Write(data)
	return len(data), nil
}

func (p *paidAttempt) meterResponse(response *http.Response) (*http.Response, func()) {
	reader, writer := io.Pipe()
	observed := make(chan paidUsageObservation, 1)
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoding := response.Header.Get("Content-Encoding")
	go func() {
		defer reader.Close()
		observed <- readPaidUsage(reader, p.family, streaming, encoding, p.route.Slug)
	}()
	forwarded := *response
	forwarded.Body = io.NopCloser(io.TeeReader(response.Body, paidUsageWriter{writer: writer}))
	return &forwarded, func() {
		_ = writer.Close()
		result := <-observed
		p.measured = result.usage
		p.usageComplete = result.complete
	}
}

func readPaidUsage(reader io.Reader, family protocolwire.Format, streaming bool, encoding, route string) paidUsageObservation {
	if streaming && (encoding == "" || encoding == "identity") {
		completion, err := protocols.Stream(family, family, reader, wire.MaxBody, route, true, func([]byte) error { return nil })
		if completion == nil {
			return paidUsageObservation{}
		}
		return paidUsageObservation{usage: completion.Usage, complete: err == nil && completion.UsageComplete}
	}
	body, err := io.ReadAll(io.LimitReader(reader, wire.MaxBody+1))
	if err != nil || len(body) > wire.MaxBody {
		return paidUsageObservation{}
	}
	decoded, err := wire.Decode(body, encoding, wire.MaxBody)
	if err != nil {
		return paidUsageObservation{}
	}
	if streaming {
		return readPaidUsage(bytes.NewReader(decoded), family, true, "", route)
	}
	completion, err := protocols.Decode(family, family, decoded, route, "")
	if completion == nil {
		return paidUsageObservation{}
	}
	return paidUsageObservation{usage: completion.Usage, complete: err == nil && completion.Usage != nil}
}

func (p *paidAttempt) safeResponse(response *http.Response) (*http.Response, error) {
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoding := response.Header.Get("Content-Encoding")
	if response.StatusCode < 400 && (!streaming || encoding != "" && encoding != "identity") {
		return response, nil
	}
	if streaming && response.StatusCode < 400 {
		reader, writer := io.Pipe()
		go func() { _ = writer.CloseWithError(p.scrubStream(response.Body, writer)) }()
		safe := *response
		safe.Header = response.Header.Clone()
		safe.Header.Del("Content-Length")
		safe.ContentLength = -1
		safe.Body = reader
		return &safe, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxBody+1))
	if err != nil || len(raw) > wire.MaxBody {
		return nil, code.Refuse(502, "code_upstream_error_invalid")
	}
	decoded, err := wire.Decode(raw, response.Header.Get("Content-Encoding"), wire.MaxBody)
	if err != nil {
		return nil, code.Refuse(502, "code_upstream_error_invalid")
	}
	scrubbed, err := p.scrubError(decoded, response.StatusCode >= 400)
	if err != nil {
		return nil, code.Refuse(502, "code_upstream_error_invalid")
	}
	safe := *response
	safe.Body = io.NopCloser(bytes.NewReader(raw))
	if !bytes.Equal(scrubbed, decoded) {
		safe.Header = response.Header.Clone()
		safe.Header.Del("Content-Encoding")
		safe.Header.Del("Content-Length")
		safe.ContentLength = int64(len(scrubbed))
		safe.Body = io.NopCloser(bytes.NewReader(scrubbed))
	}
	return &safe, nil
}

func (p *paidAttempt) scrubError(data []byte, failed bool) ([]byte, error) {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: wire.MaxBody})
	if err != nil {
		if !failed {
			return data, nil
		}
		return []byte(p.sensitive.Redact(string(data))), nil
	}
	_, hasError := doc.Root().Lookup("error")
	kind, _ := doc.Root().Lookup("type")
	text, _ := kind.Text()
	if !failed && !hasError && text != "error" && text != "response.failed" && text != "response.incomplete" {
		return data, nil
	}
	scrubbed, err := p.core.server.Host.RedactFailureDocument(doc, p.sensitive)
	if err != nil {
		return nil, err
	}
	return scrubbed.Bytes(), nil
}

func (p *paidAttempt) scrubStream(reader io.Reader, writer io.Writer) error {
	var pending bytes.Buffer
	decoder := sse.NewRawDecoder(io.TeeReader(reader, &pending), wire.MaxBody)
	for {
		frame, err := decoder.Next()
		if err != nil {
			if _, writeErr := writer.Write(pending.Bytes()); writeErr != nil {
				return writeErr
			}
			if err == io.EOF {
				return nil
			}
			_, copyErr := io.Copy(writer, reader)
			return copyErr
		}
		pending.Next(len(frame.Bytes))
		data, err := p.scrubError(frame.Data, false)
		if err != nil {
			return err
		}
		raw := frame.Bytes
		if !bytes.Equal(data, frame.Data) {
			raw, err = frame.ReplaceData(0, len(frame.Data), data)
			if err != nil {
				return err
			}
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
	}
}
