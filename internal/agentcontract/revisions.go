package agentcontract

import (
	"fmt"
	"slices"

	"github.com/tyk-swe/olp/internal/prompts"
)

func NormalizeDraft(d Draft) Draft { return cloneDraft(d, true) }
func CloneRevision(v Revision) Revision {
	return Revision{ID: v.ID, PublicationID: v.PublicationID, Kind: v.Kind, ProjectID: v.ProjectID, Definition: cloneDraft(v.Definition, false), Digest: v.Digest, Current: v.Current, Labels: slices.Clone(v.Labels)}
}
func cloneDraft(d Draft, normalize bool) Draft {
	return Draft{ProjectID: d.ProjectID, Name: d.Name, Toolset: cloneToolset(d.Toolset, normalize), Agent: cloneAgent(d.Agent, normalize), Prompt: clonePrompt(d.Prompt, normalize)}
}
func cloneToolset(value *Toolset, normalize bool) *Toolset {
	if value == nil {
		return nil
	}
	return &Toolset{Tools: cloneSlice(value.Tools, normalize)}
}
func clonePrompt(value *prompts.Definition, normalize bool) *prompts.Definition {
	if value == nil {
		return nil
	}
	cloned := value.Clone()
	return &prompts.Definition{Template: cloned.Template, Variables: cloneSlice(cloned.Variables, normalize), Messages: cloned.Messages, VariableSchema: cloned.VariableSchema, DefaultParameters: cloned.DefaultParameters}
}
func cloneAgent(value *Definition, normalize bool) *Definition {
	if value == nil {
		return nil
	}
	return &Definition{Protocol: value.Protocol, Endpoint: value.Endpoint, CardDigest: value.CardDigest, OAuth: value.OAuth.Clone(), CredentialID: clonePointer(value.CredentialID), ToolsetRevisions: cloneSlice(value.ToolsetRevisions, normalize), RouteRevisions: cloneSlice(value.RouteRevisions, normalize), PricingRevisionID: value.PricingRevisionID, MaxCost: value.MaxCost, MessagePrice: value.MessagePrice, RemoteWorkBoundSeconds: clonePointer(value.RemoteWorkBoundSeconds), MaxTaskContinuations: clonePointer(value.MaxTaskContinuations), TaskDeadlineSeconds: clonePointer(value.TaskDeadlineSeconds)}
}
func clonePointer[T string | int](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
func cloneSlice[T any](values []T, normalize bool) []T {
	if normalize {
		return append([]T{}, values...)
	}
	return slices.Clone(values)
}

func ValidateRevisions(values []Revision) error {
	if len(values) > 8192 {
		return fmt.Errorf("too many published agent revisions")
	}
	seen := map[string]bool{}
	current := map[string]bool{}
	for _, v := range values {
		if !validID(v.ID) || !validID(v.PublicationID) || seen[v.ID] || v.ProjectID != v.Definition.ProjectID || v.Digest != DraftDigest(v.Definition) {
			return fmt.Errorf("invalid published agent revision")
		}
		seen[v.ID] = true
		if err := v.Definition.Validate(v.Kind); err != nil {
			return err
		}
		if v.Current {
			if current[v.PublicationID] {
				return fmt.Errorf("duplicate current agent revision")
			}
			current[v.PublicationID] = true
		}
		labels := map[string]bool{}
		for _, label := range v.Labels {
			if v.Kind != "prompt" || (label != "production" && label != "staging") || labels[label] {
				return fmt.Errorf("invalid prompt label")
			}
			labels[label] = true
		}
	}
	return nil
}
