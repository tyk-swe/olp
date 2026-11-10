package agents

import (
	"context"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/tasks"
)

func (s *Executor) pinnedRevision(ctx context.Context, authority keyauthority.Authority, publication agentcontract.Revision, task tasks.Task, call Call, version string) (agentcontract.Revision, bool) {
	if len(call.Revisions) == 0 {
		return agentcontract.Revision{}, false
	}
	for _, revision := range call.Revisions {
		if revision.ID == task.RevisionID && revision.PublicationID == publication.PublicationID && revision.ProjectID == publication.ProjectID && revision.Definition.Agent != nil && revision.Definition.Agent.Protocol == version && s.taskBindingMatches(authority, revision, task.Binding) && call.authorized(ctx, revision) {
			return revision, true
		}
	}
	return agentcontract.Revision{}, false
}

func (s *Executor) taskBindingMatches(authority keyauthority.Authority, revision agentcontract.Revision, binding string) bool {
	if revision.Definition.Agent.OAuth == nil {
		return binding == ""
	}
	if binding == "" || s.grants == nil {
		return false
	}
	if authority.DelegationPairDigest != "" {
		return true
	}
	expected, err := s.grants.AgentBinding(authority, revision.PublicationID, *revision.Definition.Agent)
	return err == nil && expected == binding
}

type preparedSend struct {
	Params             protocol.SendMessageRequest
	Revision           agentcontract.Revision
	ExpectedUpstreamID string
	PublicTaskID       string
	Binding            string
}

func (s *Executor) preparedCredential(ctx context.Context, authority keyauthority.Authority, prepared preparedSend) ([]byte, string, error) {
	if prepared.PublicTaskID == "" {
		return s.agentCredential(ctx, authority, prepared.Revision)
	}
	credential, err := s.agentTaskCredential(ctx, authority, prepared.Revision, prepared.Binding)
	return credential, prepared.Binding, err
}

func (s *Executor) prepareSend(ctx context.Context, authority keyauthority.Authority, revision agentcontract.Revision, call Call, version string, params protocol.SendMessageRequest) (preparedSend, error) {
	if params.Message == nil || params.Message.ID == "" || params.Message.Role != protocol.MessageRoleUser || len(params.Message.Parts) == 0 || len(params.Message.ReferenceTasks) != 0 || params.Tenant != "" || params.Config != nil && params.Config.PushConfig != nil {
		return preparedSend{}, ErrInvalidParameters
	}
	if params.Message.TaskID == "" {
		if params.Message.ContextID != "" || revision.Definition.Agent == nil || revision.Definition.Agent.Protocol != version {
			return preparedSend{}, ErrInvalidParameters
		}
		if !remoteWorkBounded(authority, revision) {
			return preparedSend{}, ErrUnboundedWork
		}
		return preparedSend{Params: params, Revision: revision}, nil
	}
	owned, err := s.tasks.Find(ctx, authority, revision.PublicationID, string(params.Message.TaskID))
	if err != nil {
		return preparedSend{}, ErrTaskNotFound
	}
	pinned, ok := s.pinnedRevision(ctx, authority, revision, owned, call, version)
	if !ok || params.Message.ContextID != "" && params.Message.ContextID != owned.ID {
		return preparedSend{}, ErrTaskNotFound
	}
	if !remoteWorkBounded(authority, pinned) {
		return preparedSend{}, ErrUnboundedWork
	}
	translatedMessage := protocol.Message{ID: params.Message.ID, ContextID: owned.ContextID, Extensions: params.Message.Extensions, Metadata: params.Message.Metadata, Parts: params.Message.Parts, Role: params.Message.Role, TaskID: protocol.TaskID(owned.UpstreamID)}
	return preparedSend{Params: protocol.SendMessageRequest{Config: params.Config, Message: &translatedMessage, Metadata: params.Metadata}, Revision: pinned, ExpectedUpstreamID: owned.UpstreamID, PublicTaskID: owned.ID, Binding: owned.Binding}, nil
}

func remoteWorkBounded(authority keyauthority.Authority, revision agentcontract.Revision) bool {
	boundaries := authority.CostBoundaries("")
	return boundaries.Resolved && revision.Definition.Agent != nil && (!boundaries.CostBudgeted() || revision.Definition.Agent.RemoteWorkBoundSeconds != nil)
}
