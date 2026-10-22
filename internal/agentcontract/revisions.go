package agentcontract

import (
	"fmt"
	"slices"
)

// NormalizeDraft gives persistence an owned value and emits empty collections
// as arrays, including when a hand-written configuration omitted them.
func NormalizeDraft(d Draft) Draft {
	d = CloneRevision(Revision{Definition: d}).Definition
	if d.Toolset != nil && d.Toolset.Tools == nil {
		d.Toolset.Tools = []Tool{}
	}
	if d.Prompt != nil && d.Prompt.Variables == nil {
		d.Prompt.Variables = []string{}
	}
	if d.Agent != nil {
		if d.Agent.ToolsetRevisions == nil {
			d.Agent.ToolsetRevisions = []string{}
		}
		if d.Agent.RouteRevisions == nil {
			d.Agent.RouteRevisions = []string{}
		}
	}
	return d
}

func CloneRevision(v Revision) Revision {
	v.Labels = slices.Clone(v.Labels)
	d := v.Definition
	if d.Toolset != nil {
		copy := *d.Toolset
		copy.Tools = slices.Clone(copy.Tools)
		d.Toolset = &copy
	}
	if d.Agent != nil {
		copy := *d.Agent
		copy.ToolsetRevisions = slices.Clone(copy.ToolsetRevisions)
		copy.RouteRevisions = slices.Clone(copy.RouteRevisions)
		if copy.CredentialID != nil {
			id := *copy.CredentialID
			copy.CredentialID = &id
		}
		d.Agent = &copy
	}
	if d.Prompt != nil {
		copy := *d.Prompt
		copy.Variables = slices.Clone(copy.Variables)
		d.Prompt = &copy
	}
	v.Definition = d
	return v
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
