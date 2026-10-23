package gateway

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

// call owns authority, accounting and the shared attempt loop for one request.
// Route changes replace routeLeg; operation state lives independently.
type call struct {
	currentCommitments   *routing.Snapshot
	cache                *responseCacheCall
	cacheRateLease       *limits.Lease
	evaluatorGuardrailID string
	unary                *unaryCall
	semanticHeaders      http.Header
	semanticQuery        url.Values
	semanticQueryInvalid bool
	retainedMetered      bool
	retainedAccounting   bool
	anthropicFilesBeta   bool
	anthropicFileBinary  bool
	geminiFileBinary     bool
	serving              *oif.ServingIdentity
	servingSlot          string
	servingBinding       string

	request request
	family  wire.Format
	ingress string // the caller's wire surface, when it differs from family.Surface()

	media         *media.Request
	actor         string
	endUserDigest string
	keyID         string
	budgetGroupID *string
	attribution   map[string]string
	userID        string

	authority      access.Authority
	cardPinned     bool
	cardRevisionID *string
	cardRev        *usage.RoutingCardRevision
	routingInputs  *usage.RoutingInputs

	mode string

	preferences *routing.Preferences

	primary     *routing.Route
	deadline    time.Time
	planner     planner
	authorize   func(*routing.Route) *Error
	fixed       bool
	spent       int
	allowance   int
	spentCost   usage.Cost
	spentCharge usage.Cost
	seed        []byte

	visited    []string
	delegators []*routing.Route
	frames     []fallbackFrame

	fallbacks  []routing.FallbackStep
	caps       []capHold
	classified map[string]classification
	mirrors    bool

	origin   string
	parent   string
	priority string

	guardrailEngine           *guardrails.Engine
	guardrailCtx              context.Context
	guardrailMasks            *guardrails.Masking
	guardrailInput            []guardrails.Segment
	guardrailMu               sync.Mutex
	guardrailDecisions        []guardrails.Evidence
	guardrailPreparation      *targetPreparation
	guardrailAttemptActive    bool
	guardrailAttemptDecisions []guardrails.Evidence
	guardrailModels           map[[32]byte]guardrails.Verdict
	internalGuardrail         bool
	operationOverride         string
	policyDecisions           []contentpolicy.Decision

	captureContext context.Context
	capture        *export.Collector
	estimate       int64
	sizedInput     *int64

	historicalSnapshot *routing.Snapshot

	pin           *resources.Resource
	pinnedSlot    *routing.Slot
	providerState bool

	once              sync.Once
	facts             []AttemptFact
	exchange          *pinnedExchange
	failure           *Error         // terminal error decided before the attempt loop ran
	firstByte         *time.Duration // request start to the first payload byte the client received
	lease             *limits.Lease  // the API key reservation, settled once the request ends
	dispatched        bool           // at least one attempt was handed to a provider
	guardrailMetadata bool
	responseMetadata  bool
	attemptCount      int
	attemptVendor     string
	grantGeneration   int64
	sensitive         egress.Sensitive
	routeLeg

	generation generationCall
}

// routeLeg owns selection and every representation derived for one route.
type routeLeg struct {
	requestPreparation

	affinity []byte

	route *routing.Route

	attempts []routing.Attempt
	budget   int

	decisions []routing.Decision
	policy    routing.EffectivePolicy

	leg      *usage.RouteLeg
	selector string
	baseline *usage.Baseline

	planConditions []string

	costExcluded bool

	shadows []routing.Attempt
}

// generationCall owns the source, delivery and continuation of a generation.
type generationCall struct {
	parsed *wire.Request

	emit wire.Emit

	responseContract *storedResponseContract
	continuation     *continuationExecution

	responseBinding *responseStreamBinding
	generationHTTP  *generationHTTPFinalizer
}
