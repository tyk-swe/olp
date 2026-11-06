package agents_test

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/tasks"
)

type dispatchFixture struct {
	marked, aborted, unknown, finished chan struct{}
	settled                            chan tasks.Outcome
	markErr, settleErr                 error
}

func newDispatch() *dispatchFixture {
	return &dispatchFixture{marked: make(chan struct{}, 4), aborted: make(chan struct{}, 4), unknown: make(chan struct{}, 4), finished: make(chan struct{}, 4), settled: make(chan tasks.Outcome, 4)}
}
func (d *dispatchFixture) Capability() string { return "task-capability" }
func (d *dispatchFixture) MarkDispatched(context.Context) error {
	d.marked <- struct{}{}
	return d.markErr
}
func (d *dispatchFixture) Settle(_ context.Context, outcome tasks.Outcome) (string, error) {
	d.settled <- outcome
	return "public-task", d.settleErr
}
func (d *dispatchFixture) Abort(context.Context)   { d.aborted <- struct{}{} }
func (d *dispatchFixture) Unknown(context.Context) { d.unknown <- struct{}{} }
func (d *dispatchFixture) Finish(context.Context)  { d.finished <- struct{}{} }

type taskFixture struct {
	dispatch   tasks.Dispatch
	prepareErr error
	owned      tasks.Task
	owner      string
	intents    chan tasks.SendIntent
	states     chan tasks.State
}

func (s taskFixture) Find(_ context.Context, authority keyauthority.Authority, _, id string) (tasks.Task, error) {
	if authority.ID != s.owner || id != s.owned.ID {
		return tasks.Task{}, tasks.ErrNotFound
	}
	return s.owned, nil
}
func (s taskFixture) Prepare(_ context.Context, intent tasks.SendIntent) (tasks.Dispatch, error) {
	s.intents <- intent
	return s.dispatch, s.prepareErr
}
func (s taskFixture) RecordState(_ context.Context, _ string, state tasks.State) error {
	s.states <- state
	return nil
}

type peerFixture struct {
	agents.Peer
	result protocol.SendMessageResult
	err    error
	sends  chan protocol.SendMessageRequest
	get    func(*protocol.GetTaskRequest) (*protocol.Task, error)
	cancel func(*protocol.CancelTaskRequest) (*protocol.Task, error)
	stream iter.Seq2[protocol.Event, error]
}

func (p peerFixture) Send(_ context.Context, request *protocol.SendMessageRequest, capability string) (protocol.SendMessageResult, error) {
	p.sends <- *request
	if capability != "task-capability" {
		return nil, errors.New("dispatch capability missing")
	}
	return p.result, p.err
}
func (p peerFixture) Stream(ctx context.Context, request *protocol.SendMessageRequest, capability string) iter.Seq2[protocol.Event, error] {
	if p.stream != nil {
		return p.stream
	}
	return func(yield func(protocol.Event, error) bool) {
		result, err := p.Send(ctx, request, capability)
		yield(result, err)
	}
}
func (p peerFixture) Get(_ context.Context, request *protocol.GetTaskRequest) (*protocol.Task, error) {
	return p.get(request)
}
func (p peerFixture) Cancel(_ context.Context, request *protocol.CancelTaskRequest) (*protocol.Task, error) {
	return p.cancel(request)
}
func (p peerFixture) Streaming() bool { return true }
func (p peerFixture) Close()          {}

type peerFactory struct {
	peer   agents.Peer
	opened chan agentcontract.Definition
}

func (p peerFactory) Open(_ context.Context, definition agentcontract.Definition, _ []byte, _ bool) (agents.Peer, error) {
	p.opened <- definition
	return p.peer, nil
}
func fixtureCall() agents.Call {
	project := "project"
	revision := agentcontract.Revision{ID: "revision", PublicationID: "publication", ProjectID: project, Definition: agentcontract.Draft{ProjectID: project, Agent: &agentcontract.Definition{Protocol: "a2a-1.0.0", Endpoint: "https://agent.example"}}}
	return agents.Call{Authority: keyauthority.Authority{ID: "owner", ProjectID: &project}, Publication: revision, Revisions: []agentcontract.Revision{revision}, ReleaseID: "release", Version: "a2a-1.0.0", Revalidate: func(context.Context, agentcontract.Revision) bool { return true }}
}
func fixtureMessage() protocol.SendMessageRequest {
	return protocol.SendMessageRequest{Message: &protocol.Message{ID: "message", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("hello")}}}
}
func remoteTask() *protocol.Task {
	return &protocol.Task{ID: "remote-task", ContextID: "remote-context", Status: protocol.TaskStatus{State: protocol.TaskStateWorking}}
}
func TestSendDispatchOutcomes(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, test := range []struct {
			name                                      string
			prepareErr, markErr, remoteErr, settleErr error
			sent, settled, aborted, unknown           int
		}{
			{name: "accepted", sent: 1, settled: 1},
			{name: "admission refused", prepareErr: tasks.ErrTaskLimit},
			{name: "dispatch fence refused", markErr: errors.New("not ready"), aborted: 1},
			{name: "acceptance unknown", remoteErr: errors.New("connection lost"), sent: 1, unknown: 1},
			{name: "settlement unavailable", settleErr: errors.New("database unavailable"), sent: 1, settled: 1, unknown: 1},
		} {
			t.Run(test.name+map[bool]string{true: " stream", false: " unary"}[streaming], func(t *testing.T) {
				dispatch := newDispatch()
				dispatch.markErr, dispatch.settleErr = test.markErr, test.settleErr
				store := taskFixture{dispatch: dispatch, prepareErr: test.prepareErr, intents: make(chan tasks.SendIntent, 4), states: make(chan tasks.State, 4)}
				sends := make(chan protocol.SendMessageRequest, 4)
				peer := peerFixture{result: remoteTask(), err: test.remoteErr, sends: sends}
				executor := agents.New(agents.Options{Tasks: store, Peers: peerFactory{peer: peer, opened: make(chan agentcontract.Definition, 4)}})
				call := fixtureCall()
				result, err := func() (protocol.Event, error) {
					if !streaming {
						return executor.Send(t.Context(), call, fixtureMessage())
					}
					next, stop := iter.Pull2(executor.SendStream(t.Context(), call, fixtureMessage()))
					defer stop()
					event, err, ok := next()
					if !ok {
						t.Fatal("stream returned no outcome")
					}
					return event, err
				}()
				if test.name == "accepted" {
					if err != nil || result == nil || result.TaskInfo().TaskID != "public-task" || result.TaskInfo().ContextID != "public-task" {
						t.Fatalf("accepted task was not exposed with public identifiers: %v, %v", result, err)
					}
				} else if err == nil || result != nil {
					t.Fatal("failed dispatch exposed a successful result")
				}
				if len(sends) != test.sent || len(dispatch.settled) != test.settled || len(dispatch.aborted) != test.aborted || len(dispatch.unknown) != test.unknown {
					t.Fatalf("unexpected dispatch outcome: sends=%d settled=%d refunded=%d unknown=%d", len(sends), len(dispatch.settled), len(dispatch.aborted), len(dispatch.unknown))
				}
				if len(dispatch.finished) != test.sent {
					t.Fatal("dispatched request retained its rate lease")
				}
			})
		}
	}
}
func TestContinuationUsesOwnedPinnedRevision(t *testing.T) {
	call := fixtureCall()
	dispatch := newDispatch()
	store := taskFixture{dispatch: dispatch, owner: call.Authority.ID, owned: tasks.Task{ID: "public-task", UpstreamID: "remote-task", ContextID: "remote-context", RevisionID: call.Publication.ID}, intents: make(chan tasks.SendIntent, 4), states: make(chan tasks.State, 4)}
	sends, opened := make(chan protocol.SendMessageRequest, 4), make(chan agentcontract.Definition, 4)
	executor := agents.New(agents.Options{Tasks: store, Peers: peerFactory{peer: peerFixture{result: remoteTask(), sends: sends}, opened: opened}})
	request := protocol.SendMessageRequest{Message: &protocol.Message{ID: "continuation", Role: protocol.MessageRoleUser, TaskID: "public-task", ContextID: "public-task", Parts: fixtureMessage().Message.Parts}}
	current := agentcontract.Revision{ID: "new-revision", PublicationID: call.Publication.PublicationID, ProjectID: call.Publication.ProjectID, Definition: agentcontract.Draft{Agent: &agentcontract.Definition{Protocol: call.Version, Endpoint: "https://changed.example"}}}
	updated := agents.Call{Authority: call.Authority, Publication: current, Revisions: call.Revisions, Version: call.Version, ReleaseID: call.ReleaseID, Revalidate: call.Revalidate}
	if _, err := executor.Send(t.Context(), updated, request); err != nil {
		t.Fatal(err)
	}
	sent := <-sends
	if sent.Message.TaskID != protocol.TaskID(store.owned.UpstreamID) || sent.Message.ContextID != store.owned.ContextID || (<-opened).Endpoint != call.Publication.Definition.Agent.Endpoint {
		t.Fatal("continuation lost its original task identity or pinned endpoint")
	}
	if request.Message.TaskID != "public-task" || request.Message.ContextID != "public-task" {
		t.Fatal("send changed caller-owned request")
	}
	foreign := agents.Call{Authority: keyauthority.Authority{ID: "another-owner", ProjectID: call.Authority.ProjectID}, Publication: current, Revisions: call.Revisions, Version: call.Version, Revalidate: call.Revalidate}
	if _, err := executor.Send(t.Context(), foreign, request); !errors.Is(err, agents.ErrTaskNotFound) {
		t.Fatalf("foreign task was accepted: %v", err)
	}
	if len(sends) != 0 {
		t.Fatal("foreign continuation reached the peer")
	}
}
func TestStreamStopsWhenAuthorizationIsRevoked(t *testing.T) {
	active := make(chan struct{})
	call := fixtureCall()
	dispatch := newDispatch()
	store := taskFixture{dispatch: dispatch, intents: make(chan tasks.SendIntent, 4), states: make(chan tasks.State, 4)}
	peer := peerFixture{stream: func(yield func(protocol.Event, error) bool) {
		if !yield(remoteTask(), nil) {
			return
		}
		close(active)
		yield(&protocol.TaskStatusUpdateEvent{TaskID: "remote-task", ContextID: "remote-context", Status: protocol.TaskStatus{State: protocol.TaskStateCompleted}}, nil)
	}}
	executor := agents.New(agents.Options{Tasks: store, Peers: peerFactory{peer: peer, opened: make(chan agentcontract.Definition, 4)}})
	guarded := agents.Call{Authority: call.Authority, Publication: call.Publication, Revisions: call.Revisions, Version: call.Version, Revalidate: func(context.Context, agentcontract.Revision) bool {
		select {
		case <-active:
			return false
		default:
			return true
		}
	}}
	next, stop := iter.Pull2(executor.SendStream(t.Context(), guarded, fixtureMessage()))
	defer stop()
	first, err, ok := next()
	if !ok || err != nil || first.TaskInfo().TaskID != "public-task" {
		t.Fatalf("initial event unavailable: %v", err)
	}
	event, err, ok := next()
	if !ok || event != nil || !errors.Is(err, agents.ErrRevoked) {
		t.Fatalf("revoked stream leaked an event: %v, %v", event, err)
	}
	if len(store.states) != 0 || len(dispatch.settled) != 1 {
		t.Fatal("revoked stream changed task state or lost accepted settlement")
	}
}
func TestRemoteCancellationRecordsUnknownBeforeContactingPeer(t *testing.T) {
	states := make(chan tasks.State, 4)
	store := taskFixture{states: states}
	peer := peerFixture{cancel: func(request *protocol.CancelTaskRequest) (*protocol.Task, error) {
		if len(states) != 1 || <-states != tasks.Unknown || request.ID != "remote-task" {
			t.Fatal("cancellation was dispatched before recording uncertainty")
		}
		return &protocol.Task{ID: request.ID, ContextID: "remote-context", Status: protocol.TaskStatus{State: protocol.TaskStateCanceled}}, nil
	}}
	operations := agents.NewTaskOperations(store, peerFactory{peer: peer, opened: make(chan agentcontract.Definition, 4)}, nil)
	task := tasks.Remote{ID: "public-task", ProjectID: "project", UpstreamID: "remote-task", Definition: agentcontract.Draft{ProjectID: "project", Agent: &agentcontract.Definition{}}}
	result, err := operations.Cancel(t.Context(), task)
	if err != nil || result.ID != "public-task" || <-states != tasks.Canceled {
		t.Fatalf("canceled task was not persisted and projected: %v", err)
	}
}

type monitorFixture struct {
	pending []tasks.Remote
	claims  chan string
}

func (m monitorFixture) Horizon(context.Context) (string, error) { return "last", nil }
func (m monitorFixture) Page(context.Context, string, string) ([]tasks.Remote, error) {
	return m.pending, nil
}
func (m monitorFixture) Claim(_ context.Context, id string) (bool, error) {
	m.claims <- id
	return true, nil
}

type taskAuthority struct{}

func (taskAuthority) AuthorizeAgentTask(key string, _ string, _ string, _ string, _ string, _ *time.Time) error {
	if key == "revoked" {
		return errors.New("revoked")
	}
	return nil
}
func (taskAuthority) AuthorizeDelegatedAgentTask(context.Context, string, string, string, string) error {
	return nil
}
func TestMonitorCancelsOnlyExpiredOrRevokedTasks(t *testing.T) {
	now := time.Now()
	draft := agentcontract.Draft{ProjectID: "project", Agent: &agentcontract.Definition{}}
	pending := []tasks.Remote{
		{ID: "expired", KeyID: "owner", ProjectID: "project", UpstreamID: "remote", Definition: draft, Deadline: now.Add(-time.Second)},
		{ID: "revoked", KeyID: "revoked", ProjectID: "project", UpstreamID: "remote", Definition: draft, Deadline: now.Add(time.Hour)},
		{ID: "live", KeyID: "owner", ProjectID: "project", UpstreamID: "remote", Definition: draft, Deadline: now.Add(time.Hour)},
	}
	states := make(chan tasks.State, 4)
	claims := make(chan string, 4)
	peer := peerFixture{cancel: func(request *protocol.CancelTaskRequest) (*protocol.Task, error) {
		return &protocol.Task{ID: request.ID, ContextID: "context", Status: protocol.TaskStatus{State: protocol.TaskStateCanceled}}, nil
	}}
	operations := agents.NewTaskOperations(taskFixture{states: states}, peerFactory{peer: peer, opened: make(chan agentcontract.Definition, 4)}, nil)
	monitor := agents.NewMonitor(monitorFixture{pending: pending, claims: claims}, operations, taskAuthority{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); monitor.Run(ctx) }()
	for range 4 {
		select {
		case <-states:
		case <-ctx.Done():
			t.Fatal("monitor did not finish the two cancellations")
		}
	}
	cancel()
	<-done
	if len(claims) != 2 {
		t.Fatalf("claimed %d tasks, wanted expired and revoked only", len(claims))
	}
	for range 2 {
		if <-claims == "live" {
			t.Fatal("monitor canceled an authorized unexpired task")
		}
	}
}
