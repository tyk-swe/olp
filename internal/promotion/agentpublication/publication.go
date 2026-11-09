package agentpublication

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

const (
	kind               = contract.ConfigurationPlanItemKindAgentPublication
	maxPublications    = 512
	placeholderProject = "00000000-0000-4000-8000-000000000001"
)

var labelNames = []string{"production", "staging"}

type Publication struct {
	Project   string
	Draft     agentcontract.Draft
	Published *agentcontract.Draft
	Kind      Kind
}

type Kind interface {
	name() string
	credentials() Agent
}

type Agent struct{ Credential, PublishedCredential *promotion.Ref }

type Prompt struct {
	Labels map[string]agentcontract.Draft
}

type Toolset struct{}

func (Agent) name() string   { return "agent" }
func (Prompt) name() string  { return "prompt" }
func (Toolset) name() string { return "toolset" }

func (a Agent) credentials() Agent { return a }
func (Prompt) credentials() Agent  { return Agent{} }
func (Toolset) credentials() Agent { return Agent{} }

func kindOf(name string, labels map[string]agentcontract.Draft, credential, published *promotion.Ref) Kind {
	switch name {
	case "agent":
		return Agent{Credential: credential, PublishedCredential: published}
	case "prompt":
		return Prompt{Labels: labels}
	}
	return Toolset{}
}

func (p Publication) key() string { return keyOf(p.Project, p.Kind.name(), p.Draft.Name) }

func (p Publication) labels() map[string]agentcontract.Draft {
	prompt, ok := p.Kind.(Prompt)
	if !ok {
		return nil
	}
	return prompt.Labels
}

func (p Publication) refs() []promotion.Ref {
	credentials := p.Kind.credentials()
	return slices.Collect(func(yield func(promotion.Ref) bool) {
		for _, ref := range []*promotion.Ref{credentials.Credential, credentials.PublishedCredential} {
			if ref != nil && !yield(*ref) {
				return
			}
		}
	})
}

func (p Publication) definitions() []agentcontract.Draft {
	labels := p.labels()
	return slices.Collect(func(yield func(agentcontract.Draft) bool) {
		if !yield(p.Draft) || (p.Published != nil && !yield(*p.Published)) {
			return
		}
		for _, label := range labelNames {
			if draft, ok := labels[label]; ok && !yield(draft) {
				return
			}
		}
	})
}

func keyOf(project, kindName, name string) string {
	return promotion.TupleKey(strings.ToLower(project), kindName, strings.ToLower(name))
}

func byKey(a, b Publication) int { return strings.Compare(a.key(), b.key()) }

func derivedRef(key string) promotion.Ref {
	sum := sha256.Sum256([]byte(key))
	return promotion.Ref("agent/" + hex.EncodeToString(sum[:]))
}

func refOf(value *string) *promotion.Ref {
	if value == nil {
		return nil
	}
	ref := promotion.Ref(*value)
	return &ref
}
