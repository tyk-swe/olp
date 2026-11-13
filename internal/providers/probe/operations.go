package probe

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providers/connection"

	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/vendors"
)

const (
	maxProbePolls   = 40
	probePollWait   = 250 * time.Millisecond
	minProbePoll    = 50 * time.Millisecond
	maxProbePoll    = 5 * time.Second
	probeCancelWait = 5 * time.Second
)

func configuredOperation(transport connectors.Config, operation string) (operations.Dialect, bool) {
	if transport.ProfileID == "" {
		return operations.Dialect{}, false
	}
	profile, err := transport.Profile()
	if err != nil {
		return operations.Dialect{}, false
	}
	codec, ok := operationregistry.Lookup(profile.OperationDialect(operation))
	return codec, ok && codec.Operation.ID == operation
}
func ConfigurationCertifiable(transport connectors.Config, tuple connection.Capability) bool {
	if retainedapi.Operation(tuple.Operation) {
		return transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if transport.ProfileID == "gemini-live" {
		return tuple.Operation == "realtime" && tuple.Surface == "gemini" && tuple.Mode == "realtime" && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if duplexcontract.IsProfile(transport.ProfileID) {
		return tuple.Operation == "realtime" && tuple.Surface == "native" && tuple.Mode == "realtime" && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == "batch" {
		return transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if _, ok := configuredOperation(transport, tuple.Operation); ok {
		return transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	if tuple.Operation == "generation" && tuple.Surface == "native" {
		// Only a profile in a native generation dialect serves this surface.
		return transport.ProfileID != "" && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
	}
	return certifiable(transport.Kind, transport.VendorID, tuple)
}

// An explicit profile that cannot serve the default tuple, such as a dedicated
// embeddings or rerank dialect, is probed with the first unary tuple it can
// certify.
func DefaultTuple(transport connectors.Config) connection.Capability {
	if duplexcontract.IsProfile(transport.ProfileID) {
		if tuple := (connection.Capability{Operation: "realtime", Surface: "native", Mode: "realtime"}); Probeable(transport, tuple) {
			return tuple
		}
	}
	operation := Operation(transport)
	if transport.Kind == connectors.KindVertexAI && transport.Hosting() != "vertex-anthropic" {
		operation = "token_count"
	}
	tuple := connection.Capability{Operation: operation, Surface: "openai", Mode: "unary"}
	if transport.ProfileID == "" || Probeable(transport, tuple) {
		return tuple
	}
	profile, err := transport.Profile()
	if err != nil {
		return tuple
	}
	for _, op := range profile.Operations {
		for _, surface := range []string{"openai", "native", "anthropic", "gemini", "bedrock"} {
			if candidate := (connection.Capability{Operation: op, Surface: surface, Mode: "unary"}); Probeable(transport, candidate) {
				return candidate
			}
		}
	}
	return tuple
}

func Operation(transport connectors.Config) string {
	if contract, ok := vendors.Lookup(transport.VendorID); ok && contract.ProbeOperation != "" {
		return contract.ProbeOperation
	}
	return "generation"
}

func Probeable(transport connectors.Config, tuple connection.Capability) bool {
	return ConfigurationCertifiable(transport, tuple) && transport.Supports(tuple.Operation, tuple.Surface, tuple.Mode)
}

func (p *Prober) certifyOperation(ctx context.Context, t Target, model string, tuple connection.Capability, codec operations.Dialect) error {
	template, err := operationplan.Compile(operationplan.Config{Provider: t.Connection.Transport(), Model: model, Operation: tuple.Operation})
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "The native operation contract could not be compiled."}
	}
	// Certification invokes the target's native probe and validates its full
	// response. It does not certify every cross-dialect request or client contract.
	source, err := operationplan.Parse(codec.Identity.ID, codec.Probe("certification"), 1<<20)
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "The native operation probe is invalid."}
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "certification", ClientContract: operations.RawVectorClient})
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "The native operation probe cannot bind its configured defaults."}
	}
	submit, err := plan.RequestURL()
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "The native operation probe could not be addressed."}
	}
	resp, err := p.do(ctx, t, request{method: plan.Method(), path: submit, body: plan.WireBody()})
	if err != nil {
		return err
	}
	status, header, raw := resp.status, resp.header, resp.body
	if plan.Transport() == operations.TransportPoll202 && status == http.StatusAccepted {
		raw, err = p.pollOperation(ctx, t, plan, submit, header)
		if err != nil {
			return err
		}
	} else if status != http.StatusOK {
		return statusError(t, status, raw)
	}
	if _, err := plan.Decode(raw); err != nil {
		return &Failure{Reason: ProviderProtocolError, Detail: "The upstream violated its native operation result contract."}
	}
	return nil
}

func (p *Prober) pollOperation(ctx context.Context, t Target, plan *operationplan.Plan, submit string, header http.Header) ([]byte, error) {
	location, err := plan.PollLocation(submit, header)
	if err != nil {
		return nil, &Failure{Reason: ProviderProtocolError, Detail: "The upstream polling location is invalid."}
	}
	waitHeader := header
	for polls := 0; polls < maxProbePolls; polls++ {
		if polls > 0 {
			wait := probePollWait
			if waitHeader.Get("Retry-After") != "" {
				wait = min(max(upstream.RetryAfter(waitHeader.Get("Retry-After"), time.Now()), minProbePoll), maxProbePoll)
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				p.cancelOperationPoll(ctx, t, plan, location)
				return nil, Classify(ctx.Err())
			}
		}
		resp, err := p.do(ctx, t, request{method: http.MethodGet, path: location})
		if err != nil {
			if ctx.Err() != nil {
				p.cancelOperationPoll(ctx, t, plan, location)
			}
			return nil, err
		}
		status, raw := resp.status, resp.body
		waitHeader = resp.header
		switch {
		case status == http.StatusOK:
			state, stateErr := plan.PollState(raw)
			if stateErr != nil {
				return nil, &Failure{Reason: ProviderProtocolError, Detail: "The upstream violated its native operation result contract."}
			}
			if state == operations.PollSucceeded {
				return raw, nil
			}
			if state == operations.PollFailed {
				return nil, &Failure{Reason: UpstreamRejected, Detail: "The upstream operation failed."}
			}
		case status == http.StatusTooManyRequests || status >= 500:
			continue
		default:
			return nil, statusError(t, status, raw)
		}
	}
	p.cancelOperationPoll(ctx, t, plan, location)
	return nil, &Failure{Reason: GatewayTimeout, Detail: "The upstream operation did not finish within the probe deadline."}
}

func (p *Prober) cancelOperationPoll(ctx context.Context, t Target, plan *operationplan.Plan, location string) {
	method, target, ok := plan.CancelPoll(location)
	if !ok || method == "" || target == "" {
		return
	}
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeCancelWait)
	defer cancel()
	_, _, _ = p.call(cancelCtx, t, method, target, nil)
}

// Kind options include tuples available through an explicit profile, even
// when the kind's Automatic connector cannot serve them.
func OptionsForKind(kind connectors.Kind) []connection.Capability {
	var profiles []connectors.Config
	for _, profile := range connectors.Profiles() {
		if profile.Kind == kind {
			transport := connectors.Config{Kind: kind, ProfileID: profile.ID, ProfileRevision: profile.Revision, VendorID: profile.Vendor()}
			profiles = append(profiles, transport)
		}
	}
	out := []connection.Capability{}
	for _, tuple := range connection.Vocabulary() {
		profiled := slices.ContainsFunc(profiles, func(transport connectors.Config) bool {
			return Probeable(transport, tuple)
		})
		if certifiable(kind, vendors.DefaultFor(string(kind)), tuple) || profiled {
			out = append(out, tuple)
		}
	}
	return out
}

// Custom endpoints need a safe live probe. A reviewed vendor's media certifies
// costlessly through its own connector's model listing or account probe, and
// no reviewed vendor streams media.
func reviewedMedia(kind connectors.Kind, vendor, operation string) bool {
	contract, ok := vendors.Lookup(vendor)
	if !ok || contract.Connector != string(kind) || !contract.Serves(operation) {
		return false
	}
	return contract.AccountProbe != "" || contract.Discovery && vendors.MediaWire(vendor, operation) != ""
}

func certifiable(kind connectors.Kind, vendor string, c connection.Capability) bool {
	if c.Operation == "retrieval" && (kind == connectors.KindBedrock || kind == connectors.KindVertexAI || kind == connectors.KindAzureAISearch) {
		return false
	}
	if !connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode) {
		return false
	}
	switch c.Operation {
	case "batch":
		officialVendor := vendor == vendors.DefaultFor(string(kind))
		return c.Surface != "anthropic" && c.Surface != "gemini" || officialVendor
	case "generation", "token_count", "embeddings", "moderation", "rerank", "ocr", "search", "realtime", "bedrock_invoke":
		return true
	}
	if retainedapi.Operation(c.Operation) {
		return true
	}
	vendorMode := c.Mode == "unary" || c.Operation == "video_create" && c.Mode == "async"
	return kind == connectors.KindOpenAI || (reviewedMedia(kind, vendor, c.Operation) || mediaByCall(kind, vendor, c.Operation)) && vendorMode
}
