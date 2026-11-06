package agents

import (
	"context"
	"errors"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/workload"
)

func (s *Executor) agentCredential(ctx context.Context, authority keyauthority.Authority, revision agentcontract.Revision) ([]byte, string, error) {
	if revision.Definition.Agent == nil || revision.Definition.Agent.OAuth == nil {
		return nil, "", nil
	}
	if s.grants == nil {
		return nil, "", errors.New("agent grant unavailable")
	}
	if revision.Definition.Agent.OAuth.EntraOBO != nil {
		assertion, err := s.verifyAgentEntra(ctx, authority, *revision.Definition.Agent)
		if err != nil {
			return nil, "", errors.New("agent grant unavailable")
		}
		credential, binding, err := s.grants.ResolveAgentEntra(ctx, authority, revision.PublicationID, *revision.Definition.Agent, assertion)
		if err != nil || len(credential) == 0 || binding == "" {
			return nil, "", errors.New("agent grant unavailable")
		}
		return credential, binding, nil
	}
	credential, binding, err := s.grants.ResolveAgent(ctx, authority, revision.PublicationID, *revision.Definition.Agent)
	if err != nil || len(credential) == 0 || binding == "" {
		return nil, "", errors.New("agent grant unavailable")
	}
	return credential, binding, nil
}

func (s *Executor) verifyAgentEntra(ctx context.Context, authority keyauthority.Authority, agent agentcontract.Definition) (workload.UserAssertion, error) {
	if s.entra == nil {
		return workload.UserAssertion{}, ErrCredential
	}
	return s.entra.VerifyEntraUser(ctx, workload.RequestAssertion(ctx), authority, agent.OAuth.EntraOBO.Issuer, agent.OAuth.ClientID)
}

func (s *Executor) agentTaskCredential(ctx context.Context, authority keyauthority.Authority, revision agentcontract.Revision, binding string) ([]byte, error) {
	if revision.Definition.Agent == nil {
		return nil, errors.New("agent grant unavailable")
	}
	agent := *revision.Definition.Agent
	if agent.OAuth == nil && binding == "" {
		return nil, nil
	}
	if agent.OAuth == nil || binding == "" || s.grants == nil {
		return nil, errors.New("agent grant unavailable")
	}
	if agent.OAuth.EntraOBO != nil {
		if _, err := s.verifyAgentEntra(ctx, authority, agent); err != nil {
			return nil, errors.New("agent grant unavailable")
		}
	}
	return s.grants.ResolveAgentTask(ctx, binding, revision.PublicationID, revision.ProjectID, revision.ID, agent)
}
