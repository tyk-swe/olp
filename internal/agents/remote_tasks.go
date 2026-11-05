package agents

import (
	"context"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/agents/tasks"
)

type TaskStates interface {
	RecordState(context.Context, string, tasks.State) error
}
type TaskOperations struct {
	states TaskStates
	peers  Peers
	grants Grants
}

func NewTaskOperations(states TaskStates, peers Peers, grants Grants) *TaskOperations {
	return &TaskOperations{states: states, peers: peers, grants: grants}
}
func (o *TaskOperations) Get(ctx context.Context, task tasks.Remote) (*protocol.Task, error) {
	return o.perform(ctx, task, false, false)
}
func (o *TaskOperations) Cancel(ctx context.Context, task tasks.Remote) (*protocol.Task, error) {
	return o.perform(ctx, task, true, false)
}
func (o *TaskOperations) perform(ctx context.Context, task tasks.Remote, cancelTask, monitor bool) (*protocol.Task, error) {
	definition := task.Definition
	if definition.Agent == nil || definition.ProjectID != task.ProjectID {
		return nil, ErrInternal
	}
	credential, err := o.credential(ctx, task)
	defer clear(credential)
	if err != nil {
		return nil, err
	}
	peer, peerErr := o.peers.Open(ctx, *definition.Agent, credential, !monitor)
	if peerErr != nil {
		return nil, peerErr
	}
	defer peer.Close()
	return performTask(ctx, o.states, peer, tasks.Task{ID: task.ID, UpstreamID: task.UpstreamID}, nil, cancelTask, monitor)
}
func (o *TaskOperations) credential(ctx context.Context, task tasks.Remote) ([]byte, error) {
	definition := *task.Definition.Agent
	if definition.OAuth == nil && task.Binding == "" {
		return nil, nil
	}
	if definition.OAuth == nil || task.Binding == "" || o.grants == nil {
		return nil, ErrCredential
	}
	return o.grants.ResolveAgentTask(ctx, task.Binding, task.PublicationID, task.ProjectID, task.RevisionID, definition)
}
