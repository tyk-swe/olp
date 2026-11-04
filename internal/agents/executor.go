package agents

import (
	"context"
	"errors"
	"iter"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/workload"
)

type Failure string

func (f Failure) Error() string { return string(f) }

const (
	ErrInvalidParameters Failure = "Invalid parameters"
	ErrTaskNotFound      Failure = "Task not found"
	ErrUnboundedWork     Failure = "A finite budget requires a declared remote work bound"
	ErrRevoked           Failure = "Authorization revoked"
	ErrCredential        Failure = "Agent credential unavailable"
	ErrInternal          Failure = "Internal error"
	ErrStreaming         Failure = "Streaming unavailable"
	ErrInvalidStream     Failure = "Invalid upstream stream"
	ErrStreamSize        Failure = "stream event exceeds one MiB"
)

type TaskStore interface {
	Find(context.Context, access.Authority, string, string) (tasks.Task, error)
	Prepare(context.Context, tasks.SendIntent) (tasks.Dispatch, error)
	RecordState(context.Context, string, tasks.State) error
}

type Peer interface {
	Send(context.Context, *protocol.SendMessageRequest, string) (protocol.SendMessageResult, error)
	Stream(context.Context, *protocol.SendMessageRequest, string) iter.Seq2[protocol.Event, error]
	Get(context.Context, *protocol.GetTaskRequest) (*protocol.Task, error)
	Cancel(context.Context, *protocol.CancelTaskRequest) (*protocol.Task, error)
	Subscribe(context.Context, *protocol.SubscribeToTaskRequest) iter.Seq2[protocol.Event, error]
	Streaming() bool
	Close()
}
type Peers interface {
	Open(context.Context, agentcontract.Definition, []byte, bool) (Peer, error)
}
type Grants interface {
	AgentBinding(access.Authority, string, agentcontract.Definition) (string, error)
	ResolveAgent(context.Context, access.Authority, string, agentcontract.Definition) ([]byte, string, error)
	ResolveAgentEntra(context.Context, access.Authority, string, agentcontract.Definition, workload.UserAssertion) ([]byte, string, error)
	ResolveAgentTask(context.Context, string, string, string, string, agentcontract.Definition) ([]byte, error)
}
type EntraVerifier interface {
	VerifyEntraUser(context.Context, string, access.Authority, string, string) (workload.UserAssertion, error)
}
type Options struct {
	Tasks  TaskStore
	Peers  Peers
	Grants Grants
	Entra  EntraVerifier
}
type Executor struct {
	tasks  TaskStore
	peers  Peers
	grants Grants
	entra  EntraVerifier
}

func New(options Options) *Executor {
	return &Executor{tasks: options.Tasks, peers: options.Peers, grants: options.Grants, entra: options.Entra}
}

type Call struct {
	Authority   access.Authority
	Publication agentcontract.Revision
	Revisions   []agentcontract.Revision
	ReleaseID   string
	Version     string
	Revalidate  func(context.Context, agentcontract.Revision) bool
}

func (c Call) authorized(ctx context.Context, revision agentcontract.Revision) bool {
	return c.Revalidate != nil && c.Revalidate(ctx, revision)
}
func (c Call) context(parent context.Context, revision agentcontract.Revision) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !c.authorized(ctx, revision) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
func executionError(err error) error {
	if errors.Is(err, ErrStreamSize) {
		return ErrStreamSize
	}
	if errors.Is(err, ErrInvalidStream) {
		return ErrInvalidStream
	}
	return ErrInternal
}
