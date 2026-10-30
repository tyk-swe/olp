package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type paidCodeAttempt struct {
	provider      routing.Provider
	slot          routing.Slot
	connector     connectors.Config
	family        wire.Format
	route         codemode.Route
	release       *runtime.Release
	authority     access.Authority
	targetLease   *targetReservation
	supplyLeases  []*limits.Lease
	price         *usage.RoutingPrice
	discount      *usage.RoutingDiscount
	card          *usage.RoutingCardRevision
	started       time.Time
	status        int
	usage         *wire.Usage
	usageComplete bool
	stream        bool
}

func codeFamily(protocol codemode.Protocol) wire.Format {
	switch protocol {
	case codemode.ProtocolResponses:
		return wire.Responses
	case codemode.ProtocolMessages:
		return wire.Anthropic
	default:
		return wire.Chat
	}
}

func (s *Server) preparePaidCode(r *http.Request, release *runtime.Release, route codemode.Route, observation codemode.Request, operation codemode.Operation, ingress codeIngress, transport codeadapter.Transport, verified access.Authority, body []byte) (*codeAttempt, error) {
	if !route.PaidParticipation {
		return nil, codemode.Refuse(403, "code_paid_participation_disabled")
	}
	if transport != codeadapter.HTTP {
		return nil, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	ledger, supported := s.CodeLedger.(PaidCodeLedger)
	if !supported {
		return nil, codemode.Refuse(503, "code_paid_authority_unavailable")
	}
	target := route.PaidTargets[operation.Model]
	provider, exists := release.Snapshot.CodePaidConnections[route.RevisionID+":"+target.ProviderID]
	slotIndex := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
	if !exists || !provider.Enabled || provider.ProjectID == nil || *provider.ProjectID != route.ProjectID || slotIndex < 0 {
		return nil, codemode.Refuse(503, "code_paid_target_unavailable")
	}
	slot := provider.Slots[slotIndex]
	if !slot.Enabled || slot.CredentialID == nil || slot.CredentialVersion == nil || !slot.Allows(operation.Model, route.Slug, verified.ID) || s.Runtime.Eligibility(*slot.CredentialID) != routing.Eligible {
		return nil, codemode.Refuse(403, "code_paid_target_denied")
	}
	connector := provider.Connector()
	family := codeFamily(ingress.protocol)
	native, nativeOK := routing.CodePaidProtocol(provider, operation.Model)
	if !nativeOK || native != ingress.protocol {
		return nil, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	parsed, parseErr := protocols.Parse(family, body, operation.Model)
	if parseErr != nil {
		return nil, codemode.Refuse(400, "code_body_invalid")
	}
	mode := "unary"
	if parsed.Stream {
		mode = "streaming"
	}
	surface := family.Surface()
	if !provider.Supports(operation.Model, "generation", surface, mode) {
		return nil, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	demand := estimate.Walk(parsed).Estimate(estimate.ForModel(operation.Model), nil)
	now := s.now()
	inputs := s.Runtime.RoutingInputs()
	price := inputs.Price(provider.Kind, provider.ID, provider.VendorID, operation.Model, "generation", now)
	if price == nil {
		return nil, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	discount := inputs.Discount(provider.Kind, provider.ID, provider.VendorID, now)
	cost, priced := price.CostBound(demand.InputExposure(), demand.Reply(), true, now, discount)
	if !priced {
		return nil, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	card, cardKnown := inputs.RateCardRevision(&route.ProjectID, now)
	if !cardKnown {
		return nil, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	charge, chargePriced := func() (usage.Cost, bool) {
		if card == nil {
			return cost, true
		}
		return card.ChargeBound(demand.InputExposure(), demand.Reply(), true, now, cost, provider.Kind, provider.VendorID, operation.Model, "generation")
	}()
	if !chargePriced {
		return nil, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	labels, labelErr := s.parseAttribution(r, verified)
	if labelErr != nil {
		return nil, codemode.Refuse(labelErr.Status, "code_"+labelErr.Code)
	}
	permit, err := ledger.AdmitPaid(r.Context(), resources.PaidCodeAdmission{CodeAdmission: resources.CodeAdmission{Route: route, APIKeyID: verified.ID, EndUserDigest: verified.EndUserDigest, Attribution: labels, Operation: operation, PreviousResponse: observation.PreviousResponse}, Authority: verified, Provider: provider, Slot: slot})
	if err != nil {
		return nil, err
	}
	a := &codeAttempt{server: s, model: operation.Model, publicModel: operation.PublicModel(), permit: resources.CodePermit{Authority: permit.Authority, Attempt: permit.Attempt}, endpoint: codeadapter.Endpoint{Protocol: ingress.protocol, ClientPath: ingress.path}, observer: codeObserver(codeadapter.Endpoint{Protocol: ingress.protocol}), paid: &paidCodeAttempt{provider: provider, slot: slot, connector: connector, family: family, route: route, release: release, authority: permit.Authority, price: price, discount: discount, card: card, started: now, stream: parsed.Stream}, config: paidCodeConfiguration(provider)}
	ready := false
	defer func() {
		if !ready {
			a.finish(r.Context())
		}
	}()
	if err := bindCodeWorkload(&a.paid.authority, verified); err != nil {
		return nil, err
	}
	if failure := s.checkKeyAddress(r, a.paid.authority); failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_ip_not_allowed")
	}
	fresh, failure := s.identifyEndUser(r, a.paid.authority)
	if failure != nil || fresh.EndUserDigest != verified.EndUserDigest {
		return nil, codemode.Refuse(403, "code_end_user_invalid")
	}
	a.paid.authority = fresh
	a.paid.authority.Attribution = labels
	hold := costReservation{amount: cost.String(), charge: charge.String(), requestID: permit.Attempt.ID}
	lease, failure := s.Admission.reserveKeyCosted(r.Context(), &a.paid.authority, "code", demand.Tokens(), codeGenerationTimeout+time.Minute, hold, route.Slug)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	a.lease = lease
	targetLease, rejection, skip := s.Admission.reserveTarget(r.Context(), &provider, &slot, demand.Tokens(), codeGenerationTimeout+time.Minute, "code")
	if rejection != nil {
		return nil, codemode.Refuse(429, "code_paid_target_rate_limited")
	}
	if skip {
		return nil, codemode.Refuse(503, "code_limits_unavailable")
	}
	a.paid.targetLease = targetLease
	supply, err := s.reservePaidSupply(r.Context(), provider, slot, hold)
	a.paid.supplyLeases = supply
	if err != nil {
		return nil, err
	}
	ready = true
	return a, nil
}

func (s *Server) reservePaidSupply(ctx context.Context, provider routing.Provider, slot routing.Slot, hold costReservation) ([]*limits.Lease, error) {
	return s.reservePaidCaps(ctx, slices.Collect(func(yield func(cappedOwner) bool) {
		for index, owner := range attemptCaps(&routing.Route{}, &provider, &slot) {
			if index == 0 {
				continue
			}
			if !yield(owner) {
				return
			}
		}
	}), hold)
}

func (s *Server) reservePaidCaps(ctx context.Context, owners []cappedOwner, hold costReservation) ([]*limits.Lease, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	owner := owners[0]
	if owner.limits == nil {
		return s.reservePaidCaps(ctx, owners[1:], hold)
	}
	if failure := s.Admission.checkCostAccounting(ctx); failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	if failure := s.Admission.checkPendingCostAccounting(ctx, owner.id); failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	if !s.Admission.ready() {
		return nil, codemode.Refuse(503, "code_limits_unavailable")
	}
	lease, err := s.Admission.limiter.Reserve(ctx, limits.Request{CostOwnerID: owner.id, CostBasis: limits.BasisCost, LookupID: owner.lookup, DailyCostLimit: owner.limits.DailyCostLimit, MonthlyCostLimit: owner.limits.MonthlyCostLimit, CostEstimate: hold.amount, RequestID: hold.requestID, LeaseTTL: codeGenerationTimeout + time.Minute})
	if _, ok := errors.AsType[*limits.ExceededError](err); ok {
		return nil, codemode.Refuse(429, "code_budget_exhausted")
	}
	if err != nil {
		return nil, codemode.Refuse(503, "code_limits_unavailable")
	}
	tail, tailErr := s.reservePaidCaps(ctx, owners[1:], hold)
	return append([]*limits.Lease{lease}, tail...), tailErr
}

func (a *codeAttempt) providerID() string {
	if a.paid != nil {
		return a.paid.provider.ID
	}
	return a.permit.Account.ProviderID
}

func (a *codeAttempt) requestURL(query string) (string, error) {
	if a.paid == nil {
		return a.server.codeEndpoint(a.config, a.endpoint.UpstreamPath, query)
	}
	target, err := a.paid.connector.URL(a.paid.family, a.model, a.paid.stream)
	if err != nil {
		return "", codemode.Refuse(502, "code_connection_invalid")
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", codemode.Refuse(502, "code_connection_invalid")
	}
	if query != "" {
		if u.RawQuery != "" {
			return "", codemode.Refuse(400, "code_query_unsupported")
		}
		u.RawQuery = query
	}
	if _, err := a.server.egress.ValidateEndpoint(u.String()); err != nil {
		return "", codemode.Refuse(502, "code_egress_refused")
	}
	return u.String(), nil
}

func (a *codeAttempt) authorizePaid(request *http.Request, body []byte) error {
	if a.paid == nil {
		return nil
	}
	secret, generation, err := a.server.Runtime.Secret(request.Context(), a.paid.release, *a.paid.slot.CredentialID)
	if err != nil {
		return codemode.Refuse(503, "code_paid_credential_unavailable")
	}
	if err := a.paid.connector.ApplySemantic(request); err != nil {
		return codemode.Refuse(503, "code_paid_configuration_unsupported")
	}
	if _, err := a.server.auth.Apply(request.Context(), request, a.paid.connector, secret, body); err != nil {
		return codemode.Refuse(503, "code_paid_credential_unavailable")
	}
	a.auth = codemode.Authorization{CredentialID: *a.paid.slot.CredentialID, GrantGeneration: generation}
	return nil
}

func (a *codeAttempt) finishPaid(ctx context.Context) {
	p := a.paid
	observed := p.usage != nil && p.usageComplete && a.terminal && !a.conflicting
	measured := p.usage
	money := func() (string, string) {
		if !a.dispatched || measured == nil {
			return "", ""
		}
		cost, known := p.price.Cost(usage.ObservedAttemptUsage(measured, observed, "generation"), p.started, p.status >= 200 && p.status < 300, p.discount)
		if !known {
			return "", ""
		}
		if p.card == nil {
			return cost.String(), cost.String()
		}
		charge, chargeKnown := p.card.Charge(usage.ObservedAttemptUsage(measured, observed, "generation"), p.started, p.status >= 200 && p.status < 300, &cost, p.provider.Kind, p.provider.VendorID, a.model, "generation")
		if !chargeKnown {
			return cost.String(), ""
		}
		return cost.String(), charge.String()
	}
	cost, charge := money()
	a.lease.SetActualCost(cost, charge)
	for _, lease := range p.supplyLeases {
		lease.SetActualCost(cost, cost)
	}
	cardID := func() *string {
		if p.card != nil {
			return &p.card.ID
		}
		return nil
	}()
	mode := func() string {
		if p.stream {
			return "streaming"
		}
		return "unary"
	}()
	fact := AttemptFact{TargetID: p.slot.ID, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, UpstreamModel: a.model, SlotID: p.slot.ID, CredentialID: *p.slot.CredentialID, CredentialVersion: *p.slot.CredentialVersion, Price: p.price, PricingPinned: true, Discount: p.discount, CardPinned: true, cardRev: p.card, CardRevisionID: cardID, Budgets: []string{p.route.ID, p.provider.ID, p.slot.ID}, Ordinal: 1, Mode: mode, StartedAt: p.started, Duration: a.server.now().Sub(p.started), Status: p.status, Usage: measured, UsageObserved: measured != nil, UsageComplete: observed, BillingUncertain: a.dispatched && !observed}
	p.targetLease.settle(ctx, a.dispatched, a.usage.Total)
	for _, lease := range p.supplyLeases {
		settleKey(ctx, lease, a.dispatched, a.usage.Total, a.server.log)
	}
	if !a.dispatched {
		return
	}
	outcome := "failure"
	if p.status >= 200 && p.status < 300 {
		outcome = "success"
	}
	fact.Class = outcome
	if a.server.Sink != nil {
		a.server.Sink.Terminal(Envelope{RequestID: a.permit.Attempt.ID, AccountingID: a.permit.Attempt.ID, KeyID: p.authority.ID, EndUserDigest: p.authority.EndUserDigest, BudgetGroupID: p.authority.BudgetGroupID, Attribution: p.authority.Attribution, Route: p.route.Slug, RouteRevisionID: p.route.RevisionID, RuntimeGenerationID: p.release.Snapshot.Generation.ID, ReleaseSequence: p.release.Sequence, Family: string(p.family), Operation: "generation", Surface: p.family.Surface(), Mode: fact.Mode, Outcome: outcome, Status: p.status, Committed: true, StartedAt: p.started, CompletedAt: a.server.now(), Duration: a.server.now().Sub(p.started), Usage: measured, Attempts: []AttemptFact{fact}})
	}
}

func paidCodeConfiguration(provider routing.Provider) routing.Configuration {
	config := routing.Configuration{Kind: provider.Kind, Endpoint: provider.Endpoint}
	config.Options.Network = provider.Network
	return config
}
