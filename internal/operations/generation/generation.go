// Package generation owns ordered generation views without depending on a wire
// dialect, provider, transport, or unrelated operation family.
package generation

import "github.com/tyk-swe/olp/internal/oif"

func Contract() oif.Identity { return oif.Identity{ID: "generation", Revision: "1"} }

type Dependency struct{ Kind, ID string }
type Node struct {
	ID, Kind, Pointer string
	Source            oif.Value
	Text              string
	CallID, Name      string
	Arguments         oif.Value
	Dependencies      []Dependency
	Children          []Node
}
type Message struct {
	ID, Role, Scope, Pointer string
	Source                   oif.Value
	Nodes                    []Node
}
type Control struct {
	Name, Pointer      string
	Dialect            oif.Identity
	Value              oif.Value
	Presence           oif.Presence
	Origin             oif.Origin
	Units, BudgetScope string
}
type Tool struct {
	ID, Name, Description, Pointer  string
	Source, SchemaValue, Strictness oif.Value
	SchemaDialect                   string
}
type Candidate struct {
	ID, Pointer, FinishReason string
	Index                     int
	Nodes                     []Node
	Source                    oif.Value
}

// Request/Result views preserve native nodes alongside interpreted ones.
// Their source spans remain immutable; editing a view cannot edit an envelope.
type Request struct {
	Messages []Message
	Controls []Control
	Tools    []Tool
}

func (Request) Schema() oif.Identity { return Contract() }

type Result struct {
	Candidates []Candidate
	Usage      oif.Value
}

func (Result) Schema() oif.Identity { return Contract() }

type Event struct {
	Name, Pointer, CandidateID, ItemID string
	Sequence                           uint64
	Source                             oif.Value
}

func (Event) Schema() oif.Identity { return Contract() }
