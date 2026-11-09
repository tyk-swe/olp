package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

var (
	ErrProviderConflict = errors.New("provider batch identity changed")
	ErrStorageScope     = errors.New("cloud batch output is outside configured storage")
	ErrNoEvidence       = errors.New("provider offers no reconciliation evidence for this submission")
)

type Target struct {
	Config connectors.Config
	Model  string
	Secret []byte
	Client *http.Client
}

type Snapshot struct {
	ID           string
	Status       string
	Endpoint     string
	InputFileID  string
	OutputFileID string
	ErrorFileID  string
	Expected     *int64
	CreatedAt    time.Time
	Body         []byte
	Terminal     bool
}

type Adoption struct {
	State    string
	Metadata json.RawMessage
}

type KnownBatch func(context.Context, string) (bool, error)

type Reader interface {
	Poll(ctx context.Context, settlement batches.Settlement, target Target) (Snapshot, error)
	Reconcile(ctx context.Context, settlement batches.Settlement, target Target, known KnownBatch) ([]Snapshot, error)
	ReadResults(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (bool, error)
	Adopt(settlement batches.Settlement, snapshot Snapshot) (Adoption, error)
}

type Readers struct {
	transport transport
}

func New(auth *connectors.Auth, policy *egress.Policy) Readers {
	return Readers{transport: transport{auth: auth, egress: policy}}
}

func (r Readers) For(adapter connectors.BatchAdapter) (Reader, bool) {
	switch adapter {
	case connectors.BatchOpenAI:
		return filesReader{transport: r.transport, flavor: openAIFlavor}, true
	case connectors.BatchTogether:
		return filesReader{transport: r.transport, flavor: togetherFlavor}, true
	case connectors.BatchMistral:
		return filesReader{transport: r.transport, flavor: mistralFlavor}, true
	case connectors.BatchGemini, connectors.BatchGeminiManaged:
		return geminiReader{transport: r.transport}, true
	case connectors.BatchAnthropic:
		return anthropicReader{transport: r.transport}, true
	case connectors.BatchVertex:
		return vertexReader{transport: r.transport}, true
	case connectors.BatchBedrock:
		return bedrockReader{transport: r.transport}, true
	default:
		return nil, false
	}
}
