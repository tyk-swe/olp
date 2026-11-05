package agents

import (
	"context"
	"errors"
	"iter"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/tasks"
)

type ownedTask struct {
	task     tasks.Task
	revision agentcontract.Revision
}

func (e *Executor) ownedTask(ctx context.Context, call Call, id string) (ownedTask, error) {
	task, err := e.tasks.Find(ctx, call.Authority, call.Publication.PublicationID, id)
	if err != nil {
		return ownedTask{}, ErrTaskNotFound
	}
	revision, ok := e.pinnedRevision(ctx, call.Authority, call.Publication, task, call, call.Version)
	if !ok {
		return ownedTask{}, ErrTaskNotFound
	}
	return ownedTask{task: task, revision: revision}, nil
}
func (e *Executor) taskPeer(ctx context.Context, call Call, owned ownedTask) (Peer, error) {
	credential, err := e.agentTaskCredential(ctx, call.Authority, owned.revision, owned.task.Binding)
	defer clear(credential)
	if err != nil {
		return nil, ErrCredential
	}
	if !call.authorized(ctx, owned.revision) {
		return nil, ErrTaskNotFound
	}
	peer, openErr := e.peers.Open(ctx, *owned.revision.Definition.Agent, credential, true)
	if openErr != nil {
		return nil, ErrInternal
	}
	if !call.authorized(ctx, owned.revision) {
		peer.Close()
		return nil, ErrTaskNotFound
	}
	return peer, nil
}
func (e *Executor) Get(ctx context.Context, call Call, params protocol.GetTaskRequest) (*protocol.Task, error) {
	return e.task(ctx, call, params, false)
}
func (e *Executor) Cancel(ctx context.Context, call Call, params protocol.CancelTaskRequest) (*protocol.Task, error) {
	return e.task(ctx, call, protocol.GetTaskRequest{ID: params.ID, Tenant: params.Tenant}, true)
}
func (e *Executor) task(parent context.Context, call Call, params protocol.GetTaskRequest, cancelTask bool) (*protocol.Task, error) {
	if params.ID == "" || params.Tenant != "" {
		return nil, ErrInvalidParameters
	}
	owned, err := e.ownedTask(parent, call, string(params.ID))
	if err != nil {
		return nil, err
	}
	ctx, cancel := call.context(parent, owned.revision)
	defer cancel()
	peer, peerErr := e.taskPeer(ctx, call, owned)
	if peerErr != nil {
		return nil, peerErr
	}
	defer peer.Close()
	result, taskErr := performTask(ctx, e.tasks, peer, owned.task, params.HistoryLength, cancelTask, false)
	if taskErr != nil {
		return nil, taskErr
	}
	if !call.authorized(ctx, owned.revision) {
		return nil, ErrRevoked
	}
	return result, nil
}
func performTask(ctx context.Context, store TaskStates, peer Peer, task tasks.Task, history *int, cancelTask, terminalOnly bool) (*protocol.Task, error) {
	if cancelTask {
		if err := store.RecordState(ctx, task.ID, tasks.Unknown); err != nil && !errors.Is(err, tasks.ErrNotFound) {
			return nil, err
		}
	}
	remote, err := func() (*protocol.Task, error) {
		if cancelTask {
			return peer.Cancel(ctx, &protocol.CancelTaskRequest{ID: protocol.TaskID(task.UpstreamID)})
		}
		return peer.Get(ctx, &protocol.GetTaskRequest{ID: protocol.TaskID(task.UpstreamID), HistoryLength: history})
	}()
	if err != nil || remote == nil || string(remote.ID) != task.UpstreamID {
		return nil, ErrInternal
	}
	state := taskState(remote.Status.State)
	if terminalOnly && state != tasks.Completed && state != tasks.Failed && state != tasks.Canceled {
		return nil, ErrInternal
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.RecordState(bounded, task.ID, state); err != nil {
		return nil, err
	}
	return publicTask(remote, task.ID), nil
}
func (e *Executor) Subscribe(parent context.Context, call Call, params protocol.SubscribeToTaskRequest) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		if params.ID == "" || params.Tenant != "" {
			yield(nil, ErrInvalidParameters)
			return
		}
		owned, err := e.ownedTask(parent, call, string(params.ID))
		if err != nil {
			yield(nil, err)
			return
		}
		ctx, cancel := call.context(parent, owned.revision)
		defer cancel()
		peer, peerErr := e.taskPeer(ctx, call, owned)
		if peerErr != nil {
			yield(nil, peerErr)
			return
		}
		defer peer.Close()
		if !peer.Streaming() {
			yield(nil, ErrStreaming)
			return
		}
		next, stop := iter.Pull2(peer.Subscribe(ctx, &protocol.SubscribeToTaskRequest{ID: protocol.TaskID(owned.task.UpstreamID)}))
		defer stop()
		first, eventErr, ok := next()
		if !ok || eventErr != nil {
			yield(nil, executionError(eventErr))
			return
		}
		if !e.forwardEvent(ctx, call, owned.revision, owned.task.UpstreamID, owned.task.ContextID, owned.task.ID, first, yield) {
			return
		}
		e.forward(ctx, call, owned.revision, owned.task.UpstreamID, owned.task.ContextID, owned.task.ID, next, yield)
	}
}
func (e *Executor) forward(ctx context.Context, call Call, revision agentcontract.Revision, upstreamID, contextID, publicID string, next func() (protocol.Event, error, bool), yield func(protocol.Event, error) bool) {
	for {
		event, err, ok := next()
		if !ok {
			return
		}
		if !call.authorized(ctx, revision) {
			yield(nil, ErrRevoked)
			return
		}
		if err != nil {
			yield(nil, executionError(err))
			return
		}
		if !e.forwardEvent(ctx, call, revision, upstreamID, contextID, publicID, event, yield) {
			return
		}
	}
}
func (e *Executor) forwardEvent(ctx context.Context, call Call, revision agentcontract.Revision, upstreamID, contextID, publicID string, event protocol.Event, yield func(protocol.Event, error) bool) bool {
	if !validInitialStreamEvent(event, true) || !streamEventMatches(event, upstreamID, contextID) {
		yield(nil, ErrInvalidStream)
		return false
	}
	if !call.authorized(ctx, revision) {
		yield(nil, ErrRevoked)
		return false
	}
	switch event.(type) {
	case *protocol.Task, *protocol.TaskStatusUpdateEvent:
		if e.tasks.RecordState(ctx, publicID, taskEventState(event)) != nil {
			yield(nil, ErrInternal)
			return false
		}
	}
	return yield(publicStreamEvent(event, publicID), nil)
}
