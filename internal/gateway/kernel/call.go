package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/surface"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

// Call owns authority, accounting and the shared attempt loop for one request.
// Route changes replace routeLeg; operation state lives independently.
type Call struct {
	request request
	family  wire.Format

	callIdentity
	callLineage
	callWork

	// Sub-operation attachments.
	guard                 guardrailCall
	cache                 *responseCacheCall
	cacheRateLease        *admission.Lease
	sessionDebit          *sessionDebit
	retained              retainedOperation
	retainedAdmission     *RetainedAdmissionPlan
	strictServing         func(targetID string) (oif.ServingIdentity, bool)
	workflowRecovery      []guardrails.Segment
	workflowRecoveryCalls []string

	// Terminal recording state initialized or inspected directly by tests/surfaces.
	Failure *fault.Error

	callPinning
	callDelivery
}

type callIdentity struct {
	actor         string
	keyID         string
	budgetGroupID *string
	authority     keyauthority.Authority
	attribution   map[string]string
	endUserDigest string
	userID        string
}

type callLineage struct {
	origin               usage.Origin
	parent               string
	priority             string
	evaluatorGuardrailID string
	internalModel        bool
}

type callWork struct {
	mode               string
	ingress            string
	operationOverride  string
	prompt             *usage.PromptReference
	preferences        *routing.Preferences
	primary            *routing.Route
	deadline           time.Time
	authorize          func(*routing.Route) *fault.Error
	historicalSnapshot *routing.View
	workflow           *workflowCall
	unary              *unaryCall
	facts              []events.AttemptFact
	callSemantics
	callPricing
	callBudget
	routeLeg
	generation generationCall
}

func newCall(req request, family wire.Format, caller callIdentity, lineage callLineage, work callWork) *Call {
	return &Call{request: req, family: family, callIdentity: caller, callLineage: lineage, callWork: work}
}

const noFamily wire.Format = ""

// callSemantics holds parsed caller headers and query parameters for strict/native routes.
type callSemantics struct {
	semanticHeaders      http.Header
	semanticQuery        url.Values
	semanticQueryInvalid bool
}

func readSemantics(r *http.Request) callSemantics {
	query, err := url.ParseQuery(r.URL.RawQuery)
	return callSemantics{semanticHeaders: semanticHeaders(r.Header), semanticQuery: query, semanticQueryInvalid: err != nil}
}

// callPricing holds rate-card and routing-input snapshots pinned to the call.
type callPricing struct {
	currentCommitments *routing.View
	cardPinned         bool
	cardRevisionID     *string
	cardRev            *rates.RoutingCardRevision
	routingInputs      *routinginputs.Inputs
	fixedUsage         *FixedUsageBound
}

// callBudget tracks attempt budgets, walk progress, fallback history, and cost caps.
type callBudget struct {
	planner       planner
	customPlanner func(context.Context, *Call) *fault.Error
	fixed         bool
	spent         int
	allowance     int
	spentCost     rates.Cost
	spentCharge   rates.Cost
	seed          []byte
	walk          routing.RouteWalk
	fallbacks     []routing.FallbackStep
	caps          []capHold
	classified    map[string]classification
	mirrors       bool
}

// callPinning holds resource pinning and serving identity state.
type callPinning struct {
	PinnedHeaders  http.Header
	PinnedBinary   bool
	serving        *oif.ServingIdentity
	servingSlot    string
	servingBinding string
	pin            *resources.Resource
	pinnedSlot     *routing.Slot
	providerState  bool
	pinned         []*PinnedExchange
}

// callDelivery holds per-request delivery, redaction, capture, and settlement state.
type callDelivery struct {
	once             sync.Once
	capture          *capture.Collector
	estimate         int64
	sizedInput       *int64
	firstByte        *time.Duration
	lease            *admission.Lease
	dispatched       bool
	stepDispatched   bool
	responseMetadata bool
	attemptCount     int
	attemptVendor    string
	grantGeneration  int64
	sensitive        egress.Sensitive
}

// routeLeg owns selection and every representation derived for one route.
type routeLeg struct {
	prepared       *requestPreparation
	affinity       []byte
	Route          *routing.Route
	attempts       []routing.Attempt
	budget         int
	decisions      []routing.Decision
	policy         routing.EffectivePolicy
	leg            *usage.RouteLeg
	selector       string
	baseline       *usage.Baseline
	planConditions []string
	costExcluded   bool
	shadows        []routing.Attempt
}

// generationCall owns the source, delivery and continuation of a generation.
type generationCall struct {
	runLimits        limits.WorkflowBounds
	callerRunLimits  limits.WorkflowBounds
	parsed           *wire.Request
	approval         json.RawMessage
	resources        *generationResources
	firstStep        *modelStepAccounting
	toolsets         []string
	toolsetsSelected bool
	emit             wire.Emit
	responseContract *StoredResponseContract
	continuation     *continuationExecution
	responseBinding  *responseStreamBinding
	generationHTTP   *generationHTTPFinalizer
}

type modelStepAccounting struct {
	factEnd  int
	estimate int64
}

func (x *Call) dropIngressQuery() *fault.Error {
	badParam, message := surface.StripNonSemanticQuery(x.clientSurface(), x.semanticQuery)
	if badParam != "" {
		return fault.InvalidRequest("invalid_request", message, &badParam)
	}
	return nil
}

func (x *Call) Family() wire.Format { return x.family }

func (x *Call) SetFamily(family wire.Format) { x.family = family }

func (x *Call) SetMode(mode string) { x.mode = mode }

func (x *Call) UnaryFamily(family wire.Format) {
	x.family = family
	x.mode = "unary"
}

func (x *Call) SetEstimate(tokens int64, sized *int64) {
	x.estimate = tokens
	if sized != nil {
		x.sizedInput = sized
	}
}

func (x *Call) Estimate() int64 { return x.estimate }

func (x *Call) SetAffinity(affinity []byte) { x.affinity = affinity }

func (x *Call) SetStrictServing(fn func(targetID string) (oif.ServingIdentity, bool)) {
	x.strictServing = fn
}

func (x *Call) SetFixedUsage(bound FixedUsageBound) { x.fixedUsage = &bound }

func (x *Call) SemanticHeaders() http.Header { return x.semanticHeaders }

func (x *Call) SemanticQuery() (url.Values, bool) { return x.semanticQuery, x.semanticQueryInvalid }

func (x *Call) MarkDispatched() { x.dispatched = true }

func (x *Call) NoteDispatchFailure(failure *AttemptFailure) {
	x.dispatched = failure == nil || failure.dispatched
}

func (x *Call) NoteAttempt(fact events.AttemptFact) {
	x.facts = append(x.facts, fact)
	x.attemptCount, x.attemptVendor = len(x.facts), fact.VendorID
}

func (x *Call) RecordAttempt(fact *events.AttemptFact, dispatched bool) {
	x.dispatched = x.dispatched || dispatched
	if fact != nil {
		x.facts = append(x.facts, *fact)
	}
}

func (x *Call) SummarizeAttemptVendors() {
	x.attemptCount, x.attemptVendor = len(x.facts), ""
	if len(x.facts) == 0 {
		return
	}
	x.attemptVendor = x.facts[0].VendorID
	for _, fact := range x.facts[1:] {
		if fact.VendorID != x.attemptVendor {
			x.attemptVendor = ""
			return
		}
	}
}

func (x *Call) RouteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return x.routeContext(ctx)
}

func (x *Call) StartAttemptTrace(ctx context.Context, providerKind, revisionID, model string) (context.Context, *telemetry.AttemptTrace) {
	return x.request.trace.Attempt(ctx, providerKind, revisionID, model)
}

func (x *Call) SensitiveRef() *egress.Sensitive { return &x.sensitive }

func (x *Call) IsCallerCredential(provider *routing.Provider) bool {
	return provider.CredentialSource == "caller" && x.origin != "probe"
}
