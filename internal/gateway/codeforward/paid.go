package codeforward

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/usage/rates"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/release"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type paidCodeAttempt struct {
	sensitive     egress.Sensitive
	provider      routing.Provider
	slot          routing.Slot
	connector     connectors.Config
	family        wire.Format
	route         codemode.Route
	release       *release.Release
	authority     keyauthority.Authority
	targetLease   *quota.TargetReservation
	demand        quota.TokenDemand
	supplyLeases  []*admission.Lease
	price         *rates.RoutingPrice
	discount      *rates.RoutingDiscount
	card          *rates.RoutingCardRevision
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

type paidCodePricing struct {
	provider routing.Provider
	slot     routing.Slot
	family   wire.Format
	stream   bool
	demand   estimate.Estimate
	now      time.Time
	price    *rates.RoutingPrice
	discount *rates.RoutingDiscount
	card     *rates.RoutingCardRevision
	cost     rates.Cost
	charge   rates.Cost
}

func (f *Forwarder) resolvePaidCodeTarget(req codeRequest, operation codemode.Operation, verified keyauthority.Authority, fallback bool) (paidCodePricing, error) {
	if !req.route.PaidParticipation {
		return paidCodePricing{}, codemode.Refuse(403, "code_paid_participation_disabled")
	}
	if req.transport != codeadapter.HTTP || operation.Name == "compact" || operation.Name == "prewarm" {
		return paidCodePricing{}, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	target := req.route.PaidTarget(operation.Model, fallback)
	provider, exists := req.release.Snapshot().FindCodePaidConnection(req.route.RevisionID + ":" + target.ProviderID)
	slotIndex := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
	if !exists || !provider.Enabled || provider.ProjectID == nil || *provider.ProjectID != req.route.ProjectID || slotIndex < 0 {
		return paidCodePricing{}, codemode.Refuse(503, "code_paid_target_unavailable")
	}
	slot := provider.Slots[slotIndex]
	if !slot.Enabled || slot.CredentialID == nil || slot.CredentialVersion == nil || !slot.Allows(operation.Model, req.route.Slug, verified.ID) || f.runtime.Eligibility(*slot.CredentialID) != routing.Eligible {
		return paidCodePricing{}, codemode.Refuse(403, "code_paid_target_denied")
	}
	family := codeFamily(req.inbound.protocol)
	native, nativeOK := routing.CodePaidProtocol(provider, operation.Model)
	if !nativeOK || native != req.inbound.protocol {
		return paidCodePricing{}, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	parsed, parseErr := protocols.ParseForModel(family, req.body, operation.PublicModel())
	if parseErr != nil {
		return paidCodePricing{}, codemode.Refuse(400, "code_body_invalid")
	}
	mode := "unary"
	if parsed.Stream {
		mode = "streaming"
	}
	if !provider.Supports(operation.Model, "generation", family.Surface(), mode) {
		return paidCodePricing{}, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	demand := estimate.Walk(parsed).Estimate(estimate.ForModel(operation.Model), nil)
	now := f.now()
	inputs := f.runtime.RoutingInputs()
	price := inputs.Price(string(provider.Kind), provider.ID, provider.VendorID, operation.Model, "generation", now)
	if price == nil {
		return paidCodePricing{}, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	discount := inputs.Discount(string(provider.Kind), provider.ID, provider.VendorID, now)
	cost, priced := price.CostBound(demand.InputExposure(), demand.Reply(), true, now, discount)
	if !priced {
		return paidCodePricing{}, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	card, cardKnown := inputs.RateCardRevision(&req.route.ProjectID, now)
	if !cardKnown {
		return paidCodePricing{}, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	charge, chargePriced := cost, true
	if card != nil {
		charge, chargePriced = card.ChargeBound(demand.InputExposure(), demand.Reply(), true, now, cost, string(provider.Kind), provider.VendorID, operation.Model, "generation")
	}
	if !chargePriced {
		return paidCodePricing{}, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	return paidCodePricing{provider: provider, slot: slot, family: family, stream: parsed.Stream, demand: demand, now: now, price: price, discount: discount, card: card, cost: cost, charge: charge}, nil
}

func (f *Forwarder) preparePaidCode(req codeRequest, operation codemode.Operation, verified keyauthority.Authority, recovery *codesessions.Recovery, fallback bool) (*codeAttempt, error) {
	priced, err := f.resolvePaidCodeTarget(req, operation, verified, fallback)
	if err != nil {
		return nil, err
	}
	labels, labelErr := ingress.ParseAttribution(req.r, verified)
	if labelErr != nil {
		return nil, codemode.Refuse(labelErr.Status, "code_"+labelErr.Code)
	}
	permit, err := f.ledger.AdmitPaid(req.r.Context(), codesessions.PaidAdmission{Admission: codesessions.Admission{Route: req.route, APIKeyID: verified.ID, EndUserDigest: verified.EndUserDigest, Attribution: labels, Operation: operation, PreviousResponse: req.observation.PreviousResponse, InitialRecovery: req.observation.Recoverable, Recovery: recovery, ModelSource: req.release.Snapshot(), ModelPath: req.inbound.path, ModelTransport: req.transport}, Fallback: fallback, Authority: verified, Provider: priced.provider, Slot: priced.slot})
	if err != nil {
		return nil, err
	}
	a := &codeAttempt{forwarder: f, model: operation.Model, publicModel: operation.PublicModel(), permit: codesessions.Permit{Authority: permit.Authority, Attempt: permit.Attempt}, endpoint: codeadapter.Endpoint{Protocol: req.inbound.protocol, ClientPath: req.inbound.path}, observer: codeObserver(codeadapter.Endpoint{Protocol: req.inbound.protocol}, req.observation.ChatChoices), paid: &paidCodeAttempt{provider: priced.provider, slot: priced.slot, connector: priced.provider.Connector(), family: priced.family, route: req.route, release: req.release, authority: permit.Authority, price: priced.price, discount: priced.discount, card: priced.card, started: priced.now, stream: priced.stream}, config: paidCodeConfiguration(priced.provider)}
	ready := false
	defer func() {
		if !ready {
			a.finish(req.r.Context())
		}
	}()
	if err := f.reservePaidCodeAttempt(req.r, a, verified, labels, req.route, priced, permit.Attempt.ID); err != nil {
		return nil, err
	}
	ready = true
	return a, nil
}

func (f *Forwarder) reservePaidCodeAttempt(r *http.Request, a *codeAttempt, verified keyauthority.Authority, labels map[string]string, route codemode.Route, priced paidCodePricing, attemptID string) error {
	if err := bindCodeWorkload(&a.paid.authority, verified); err != nil {
		return err
	}
	if failure := f.gate.CheckKeyAddress(r, a.paid.authority); failure != nil {
		return codemode.Refuse(failure.Status, "code_ip_not_allowed")
	}
	fresh, failure := f.gate.IdentifyEndUser(r, a.paid.authority)
	if failure != nil || fresh.EndUserDigest != verified.EndUserDigest {
		return codemode.Refuse(403, "code_end_user_invalid")
	}
	a.paid.authority = fresh
	a.paid.authority.Attribution = labels
	hold := quota.CostReservation{Amount: priced.cost.String(), Charge: priced.charge.String(), RequestID: attemptID}
	lease, failure := f.admission.ReserveKeyCosted(r.Context(), &a.paid.authority, "code", priced.demand.Tokens(), codeGenerationTimeout+time.Minute, hold, route.Slug)
	if failure != nil {
		return codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	a.lease = lease
	a.paid.demand = quota.DemandOf(priced.demand)
	targetLease, rejection, skip := f.admission.ReserveTarget(r.Context(), &priced.provider, &priced.slot, a.paid.demand, codeGenerationTimeout+time.Minute, "code", "", 0)
	if rejection != nil {
		return codemode.Refuse(429, "code_paid_target_rate_limited")
	}
	if skip {
		return codemode.Refuse(503, "code_limits_unavailable")
	}
	a.paid.targetLease = targetLease
	supply, err := f.reservePaidSupply(r.Context(), priced.provider, priced.slot, hold)
	a.paid.supplyLeases = supply
	return err
}

func (f *Forwarder) reservePaidSupply(ctx context.Context, provider routing.Provider, slot routing.Slot, hold quota.CostReservation) ([]*admission.Lease, error) {
	return f.reservePaidCaps(ctx, slices.Collect(func(yield func(quota.CappedOwner) bool) {
		for index, owner := range quota.AttemptCaps(&routing.Route{}, &provider, &slot) {
			if index == 0 {
				continue
			}
			if !yield(owner) {
				return
			}
		}
	}), hold)
}

func (f *Forwarder) reservePaidCaps(ctx context.Context, owners []quota.CappedOwner, hold quota.CostReservation) ([]*admission.Lease, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	owner := owners[0]
	if owner.Limits == nil {
		return f.reservePaidCaps(ctx, owners[1:], hold)
	}
	if failure := f.admission.CheckCostAccounting(ctx); failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	if failure := f.admission.CheckPendingCostAccounting(ctx, owner.ID); failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	if !f.admission.Ready() {
		return nil, codemode.Refuse(503, "code_limits_unavailable")
	}
	lease, err := f.admission.Reserve(ctx, hold.On(quota.CapRequest(owner, codeGenerationTimeout+time.Minute)))
	if _, ok := errors.AsType[*limits.ExceededError](err); ok {
		return nil, codemode.Refuse(429, "code_budget_exhausted")
	}
	if err != nil {
		return nil, codemode.Refuse(503, "code_limits_unavailable")
	}
	tail, tailErr := f.reservePaidCaps(ctx, owners[1:], hold)
	return append([]*admission.Lease{lease}, tail...), tailErr
}

func (a *codeAttempt) providerID() string {
	if a.paid != nil {
		return a.paid.provider.ID
	}
	return a.permit.Account.ProviderID
}

func (a *codeAttempt) requestURL(query string, stream bool) (string, error) {
	if a.paid == nil {
		return a.forwarder.codeEndpoint(a.config, a.endpoint.Path(stream), query)
	}
	target, err := a.paid.connector.URL(a.paid.family, a.model, a.paid.stream)
	if err != nil {
		return "", codemode.Refuse(502, "code_connection_invalid")
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", codemode.Refuse(502, "code_connection_invalid")
	}
	if _, err := a.forwarder.egress.ValidateEndpoint(u.String()); err != nil {
		return "", codemode.Refuse(502, "code_egress_refused")
	}
	if query != "" {
		if u.RawQuery != "" {
			return "", codemode.Refuse(400, "code_query_unsupported")
		}
		u.RawQuery = query
	}
	return u.String(), nil
}

func (a *codeAttempt) authorizePaid(request *http.Request, body []byte) error {
	if a.paid == nil {
		return nil
	}
	secret, generation, err := a.forwarder.runtime.Secret(request.Context(), a.paid.release, *a.paid.slot.CredentialID)
	if err != nil {
		return codemode.Refuse(503, "code_paid_credential_unavailable")
	}
	if err := a.paid.connector.ApplySemantic(request); err != nil {
		return codemode.Refuse(503, "code_paid_configuration_unsupported")
	}
	sensitive, err := a.forwarder.auth.Apply(request.Context(), request, a.paid.connector, secret, body)
	if err != nil {
		return codemode.Refuse(503, "code_paid_credential_unavailable")
	}
	a.paid.sensitive = sensitive
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
		charge, chargeKnown := p.card.Charge(usage.ObservedAttemptUsage(measured, observed, "generation"), p.started, p.status >= 200 && p.status < 300, &cost, string(p.provider.Kind), p.provider.VendorID, a.model, "generation")
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
	mode := func() string {
		if p.stream {
			return "streaming"
		}
		return "unary"
	}()
	outcome := func() string {
		if p.status >= 200 && p.status < 300 {
			return "success"
		}
		return "failure"
	}()
	fact := events.AttemptFact{VendorID: p.provider.VendorID, Class: outcome, TargetID: p.slot.ID, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, UpstreamModel: a.model, SlotID: p.slot.ID, CredentialID: *p.slot.CredentialID, CredentialVersion: *p.slot.CredentialVersion, Price: p.price, PricingPinned: true, Discount: p.discount, CardPinned: true, CardRev: p.card, Budgets: []string{p.route.ID, p.provider.ID, p.slot.ID}, Ordinal: 1, Mode: mode, StartedAt: p.started, Duration: a.forwarder.now().Sub(p.started), Status: p.status, Usage: measured, UsageObserved: measured != nil, UsageComplete: observed, BillingUncertain: a.dispatched && !observed}
	p.targetLease.Settle(ctx, a.dispatched, quota.SettledAttemptUsage(&fact, p.demand))
	for _, lease := range p.supplyLeases {
		quota.SettleKey(ctx, lease, a.dispatched, a.usage.Total, a.forwarder.log)
	}
	if !a.dispatched {
		return
	}
	if a.forwarder.sink != nil {
		a.forwarder.sink.Terminal(events.Envelope{RequestID: a.permit.Attempt.ID, AccountingID: a.permit.Attempt.ID, KeyID: p.authority.ID, EndUserDigest: p.authority.EndUserDigest, BudgetGroupID: p.authority.BudgetGroupID, Attribution: p.authority.Attribution, Route: p.route.Slug, RouteRevisionID: p.route.RevisionID, RuntimeGenerationID: p.release.Snapshot().Generation().ID, ReleaseSequence: p.release.Sequence, Family: string(p.family), Operation: "generation", Surface: p.family.Surface(), Mode: fact.Mode, Outcome: outcome, Status: p.status, Committed: true, StartedAt: p.started, CompletedAt: a.forwarder.now(), Duration: a.forwarder.now().Sub(p.started), Usage: measured, Attempts: []events.AttemptFact{fact}})
	}
}

func paidCodeConfiguration(provider routing.Provider) routing.Configuration {
	config := routing.Configuration{Identity: providerconfig.Identity[string]{Kind: provider.Kind, Endpoint: provider.Endpoint}}
	config.Options.Network = provider.Network
	return config
}
