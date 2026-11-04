package agents

import (
	"context"
	"iter"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/agents/tasks"
)

func (e *Executor) Send(ctx context.Context, call Call, params protocol.SendMessageRequest) (protocol.Event, error) {
	for event, err := range e.send(ctx, call, params, false) {
		return event, err
	}
	return nil, ErrInternal
}
func (e *Executor) SendStream(ctx context.Context, call Call, params protocol.SendMessageRequest) iter.Seq2[protocol.Event, error] {
	return e.send(ctx, call, params, true)
}
func (e *Executor) send(parent context.Context, call Call, params protocol.SendMessageRequest, streaming bool) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		prepared, err := e.prepareSend(parent, call.Authority, call.Publication, call, call.Version, params)
		if err != nil {
			yield(nil, err)
			return
		}
		if !call.authorized(parent, prepared.Revision) {
			yield(nil, ErrRevoked)
			return
		}
		ctx, cancel := call.context(parent, prepared.Revision)
		defer cancel()
		credential, binding, credentialErr := e.preparedCredential(ctx, call.Authority, prepared)
		defer clear(credential)
		if credentialErr != nil {
			yield(nil, ErrCredential)
			return
		}
		if !call.authorized(ctx, prepared.Revision) {
			yield(nil, ErrRevoked)
			return
		}
		peer, peerErr := e.peers.Open(ctx, *prepared.Revision.Definition.Agent, credential, true)
		if peerErr != nil {
			if streaming {
				yield(nil, ErrStreaming)
			} else {
				yield(nil, ErrInternal)
			}
			return
		}
		defer peer.Close()
		if streaming && !peer.Streaming() {
			yield(nil, ErrStreaming)
			return
		}
		dispatch, admissionErr := e.tasks.Prepare(ctx, tasks.SendIntent{Authority: call.Authority, Revision: prepared.Revision, ReleaseID: call.ReleaseID, TaskID: prepared.PublicTaskID, Binding: binding})
		if admissionErr != nil {
			yield(nil, admissionErr)
			return
		}
		if !call.authorized(ctx, prepared.Revision) {
			dispatch.Abort(parent)
			yield(nil, ErrRevoked)
			return
		}
		if dispatch.MarkDispatched(ctx) != nil {
			dispatch.Abort(parent)
			yield(nil, ErrInternal)
			return
		}
		defer dispatch.Finish(parent)
		next, stop := iter.Pull2(sendEvents(ctx, peer, prepared.Params, dispatch.Capability(), streaming))
		defer stop()
		first, eventErr, ok := next()
		if !ok || eventErr != nil || first == nil {
			dispatch.Unknown(parent)
			yield(nil, executionError(eventErr))
			return
		}
		outcome, valid := acceptedOutcome(first, prepared, streaming)
		if !valid {
			dispatch.Unknown(parent)
			if !streaming {
				yield(nil, ErrInternal)
				return
			}
			yield(nil, ErrInvalidStream)
			return
		}
		publicID, settleErr := settleSend(parent, dispatch, outcome)
		if settleErr != nil {
			dispatch.Unknown(parent)
			yield(nil, ErrInternal)
			return
		}
		if !call.authorized(ctx, prepared.Revision) {
			yield(nil, ErrRevoked)
			return
		}
		if !yield(publicStreamEvent(first, publicID), nil) || !streaming {
			return
		}
		e.forward(ctx, call, prepared.Revision, outcome.UpstreamID, outcome.ContextID, publicID, next, yield)
	}
}
func sendEvents(ctx context.Context, peer Peer, params protocol.SendMessageRequest, capability string, streaming bool) iter.Seq2[protocol.Event, error] {
	if streaming {
		return peer.Stream(ctx, &params, capability)
	}
	return func(yield func(protocol.Event, error) bool) {
		result, err := peer.Send(ctx, &params, capability)
		yield(result, err)
	}
}
func acceptedOutcome(event protocol.Event, prepared preparedSend, streaming bool) (tasks.Outcome, bool) {
	if !streaming {
		result, ok := event.(protocol.SendMessageResult)
		if !ok {
			return tasks.Outcome{}, false
		}
		id, contextID, valid := resultIDs(result)
		return tasks.Outcome{UpstreamID: id, ContextID: contextID, State: taskEventState(event)}, valid && (prepared.ExpectedUpstreamID == "" || id == prepared.ExpectedUpstreamID)
	}
	if !validInitialStreamEvent(event, true) {
		return tasks.Outcome{}, false
	}
	if prepared.ExpectedUpstreamID != "" && !streamEventMatches(event, prepared.ExpectedUpstreamID, prepared.Params.Message.ContextID) {
		return tasks.Outcome{}, false
	}
	info := event.TaskInfo()
	return tasks.Outcome{UpstreamID: string(info.TaskID), ContextID: info.ContextID, State: taskEventState(event)}, true
}
func settleSend(ctx context.Context, dispatch tasks.Dispatch, outcome tasks.Outcome) (string, error) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return dispatch.Settle(bounded, outcome)
}
