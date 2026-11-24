package kernel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/surface"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/platform/egress"
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
	ingress string
	mode    string

	// Caller identity and authorization.
	actor         string
	endUserDigest string
	keyID         string
	budgetGroupID *string
	attribution   map[string]string
	userID        string
	authority     keyauthority.Authority

	// Sub-operation attachments.
	prompt               *usage.PromptReference
	cache                *responseCacheCall
	cacheRateLease       *admission.Lease
	sessionDebit         *sessionDebit
	workflow             *workflowCall
	evaluatorGuardrailID string
	unary                *unaryCall
	media                *media.Request
	retained             retainedOperation
	retainedAdmission    *RetainedAdmissionPlan

	// Routing provenance and guardrails.
	preferences           *routing.Preferences
	primary               *routing.Route
	deadline              time.Time
	authorize             func(*routing.Route) *fault.Error
	origin                usage.Origin
	parent                string
	priority              string
	guard                 guardrailCall
	workflowRecovery      []guardrails.Segment
	workflowRecoveryCalls []string
	operationOverride     string
	historicalSnapshot    *routing.View

	// Terminal recording state initialized or inspected directly by tests/surfaces.
	facts   []events.AttemptFact
	Failure *fault.Error

	callSemantics
	callPricing
	callBudget
	callPinning
	callDelivery
	routeLeg

	generation generationCall
}

// callSemantics holds parsed caller headers and query parameters for strict/native routes.
type callSemantics struct {
	semanticHeaders      http.Header
	semanticQuery        url.Values
	semanticQueryInvalid bool
}

// callPricing holds rate-card and routing-input snapshots pinned to the call.
type callPricing struct {
	currentCommitments *routing.View
	cardPinned         bool
	cardRevisionID     *string
	cardRev            *rates.RoutingCardRevision
	routingInputs      *routinginputs.Inputs
}

// callBudget tracks attempt budgets, walk progress, fallback history, and cost caps.
type callBudget struct {
	planner     planner
	fixed       bool
	spent       int
	allowance   int
	spentCost   rates.Cost
	spentCharge rates.Cost
	seed        []byte
	walk        routing.RouteWalk
	fallbacks   []routing.FallbackStep
	caps        []capHold
	classified  map[string]classification
	mirrors     bool
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
