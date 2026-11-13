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
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
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

func (f *Forwarder) preparePaidCode(r *http.Request, release *release.Release, route codemode.Route, observation codemode.Request, operation codemode.Operation, inbound codeIngress, transport codeadapter.Transport, verified keyauthority.Authority, body []byte, recovery *codesessions.Recovery, fallback bool) (*codeAttempt, error) {
	if !route.PaidParticipation {
		return nil, codemode.Refuse(403, "code_paid_participation_disabled")
	}
	if transport != codeadapter.HTTP || operation.Name == "compact" || operation.Name == "prewarm" {
		return nil, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	ledger, supported := f.ledger.(PaidLedger)
	if !supported {
		return nil, codemode.Refuse(503, "code_paid_authority_unavailable")
	}
	target := route.PaidTarget(operation.Model, fallback)
	provider, exists := release.Snapshot().FindCodePaidConnection(route.RevisionID + ":" + target.ProviderID)
	slotIndex := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
	if !exists || !provider.Enabled || provider.ProjectID == nil || *provider.ProjectID != route.ProjectID || slotIndex < 0 {
		return nil, codemode.Refuse(503, "code_paid_target_unavailable")
	}
	slot := provider.Slots[slotIndex]
	if !slot.Enabled || slot.CredentialID == nil || slot.CredentialVersion == nil || !slot.Allows(operation.Model, route.Slug, verified.ID) || f.runtime.Eligibility(*slot.CredentialID) != routing.Eligible {
		return nil, codemode.Refuse(403, "code_paid_target_denied")
	}
	connector := provider.Connector()
	family := codeFamily(inbound.protocol)
	native, nativeOK := routing.CodePaidProtocol(provider, operation.Model)
	if !nativeOK || native != inbound.protocol {
		return nil, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	parsed, parseErr := protocols.ParseForModel(family, body, operation.PublicModel())
	if parseErr != nil {
		return nil, codemode.Refuse(400, "code_body_invalid")
	}
	mode := func() string {
		if parsed.Stream {
			return "streaming"
		}
		return "unary"
	}()
	surface := family.Surface()
	if !provider.Supports(operation.Model, "generation", surface, mode) {
		return nil, codemode.Refuse(400, "code_paid_operation_unsupported")
	}
	demand := estimate.Walk(parsed).Estimate(estimate.ForModel(operation.Model), nil)
	now := f.now()
	inputs := f.runtime.RoutingInputs()
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
	charge, chargePriced := func() (rates.Cost, bool) {
		if card == nil {
			return cost, true
		}
		return card.ChargeBound(demand.InputExposure(), demand.Reply(), true, now, cost, provider.Kind, provider.VendorID, operation.Model, "generation")
	}()
	if !chargePriced {
		return nil, codemode.Refuse(503, "code_paid_pricing_unavailable")
	}
	labels, labelErr := ingress.ParseAttribution(r, verified)
	if labelErr != nil {
		return nil, codemode.Refuse(labelErr.Status, "code_"+labelErr.Code)
	}
	permit, err := ledger.AdmitPaid(r.Context(), codesessions.PaidAdmission{Admission: codesessions.Admission{Route: route, APIKeyID: verified.ID, EndUserDigest: verified.EndUserDigest, Attribution: labels, Operation: operation, PreviousResponse: observation.PreviousResponse, InitialRecovery: observation.Recoverable, Recovery: recovery, ModelSource: release.Snapshot(), ModelPath: inbound.path, ModelTransport: transport}, Fallback: fallback, Authority: verified, Provider: provider, Slot: slot})
	if err != nil {
		return nil, err
	}
	a := &codeAttempt{forwarder: f, model: operation.Model, publicModel: operation.PublicModel(), permit: codesessions.Permit{Authority: permit.Authority, Attempt: permit.Attempt}, endpoint: codeadapter.Endpoint{Protocol: inbound.protocol, ClientPath: inbound.path}, observer: codeObserver(codeadapter.Endpoint{Protocol: inbound.protocol}, observation.ChatChoices), paid: &paidCodeAttempt{provider: provider, slot: slot, connector: connector, family: family, route: route, release: release, authority: permit.Authority, price: price, discount: discount, card: card, started: now, stream: parsed.Stream}, config: paidCodeConfiguration(provider)}
	ready := false
	defer func() {
		if !ready {
			a.finish(r.Context())
		}
	}()
	if err := bindCodeWorkload(&a.paid.authority, verified); err != nil {
		return nil, err
	}
	if failure := f.gate.CheckKeyAddress(r, a.paid.authority); failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_ip_not_allowed")
	}
	fresh, failure := f.gate.IdentifyEndUser(r, a.paid.authority)
	if failure != nil || fresh.EndUserDigest != verified.EndUserDigest {
		return nil, codemode.Refuse(403, "code_end_user_invalid")
	}
	a.paid.authority = fresh
	a.paid.authority.Attribution = labels
	hold := quota.CostReservation{Amount: cost.String(), Charge: charge.String(), RequestID: permit.Attempt.ID}
	lease, failure := f.admission.ReserveKeyCosted(r.Context(), &a.paid.authority, "code", demand.Tokens(), codeGenerationTimeout+time.Minute, hold, route.Slug)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	a.lease = lease
	targetLease, rejection, skip := f.admission.ReserveTarget(r.Context(), &provider, &slot, demand.Tokens(), codeGenerationTimeout+time.Minute, "code")
	if rejection != nil {
		return nil, codemode.Refuse(429, "code_paid_target_rate_limited")
	}
	if skip {
		return nil, codemode.Refuse(503, "code_limits_unavailable")
	}
	a.paid.targetLease = targetLease
	supply, err := f.reservePaidSupply(r.Context(), provider, slot, hold)
	a.paid.supplyLeases = supply
	if err != nil {
		return nil, err
	}
	ready = true
	return a, nil
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
	outcome := func() string {
		if p.status >= 200 && p.status < 300 {
			return "success"
		}
		return "failure"
	}()
	fact := events.AttemptFact{VendorID: p.provider.VendorID, Class: outcome, TargetID: p.slot.ID, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, UpstreamModel: a.model, SlotID: p.slot.ID, CredentialID: *p.slot.CredentialID, CredentialVersion: *p.slot.CredentialVersion, Price: p.price, PricingPinned: true, Discount: p.discount, CardPinned: true, CardRev: p.card, CardRevisionID: cardID, Budgets: []string{p.route.ID, p.provider.ID, p.slot.ID}, Ordinal: 1, Mode: mode, StartedAt: p.started, Duration: a.forwarder.now().Sub(p.started), Status: p.status, Usage: measured, UsageObserved: measured != nil, UsageComplete: observed, BillingUncertain: a.dispatched && !observed}
	p.targetLease.Settle(ctx, a.dispatched, a.usage.Total)
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
