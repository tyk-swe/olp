package oif

import "fmt"

// View is implemented by operation-owned interpretations. The envelope remains
// authoritative even when an operation has no portable interpretation.
type View interface{ Schema() Identity }
type Operation struct{ Identity Identity }
type RequestAdapter interface{ LiftRequest(Request) (View, error) }
type ResultAdapter interface{ LiftResult(Result) (View, error) }
type EventAdapter interface{ LiftEvent(Event) (View, error) }
type Binding struct {
	Dialect, Operation Identity
	Request            RequestAdapter
	Result             ResultAdapter
	Event              EventAdapter
	IdentityRules      []IdentityRule
}

// Registry links trusted build-time registrations. It is immutable after NewRegistry
// returns; adding a dialect or operation does not change any operation kernel.
type Registry struct {
	operations map[Identity]Operation
	bindings   map[bindingKey]Binding
}
type bindingKey struct{ dialect, operation Identity }

func NewRegistry(operations []Operation, bindings []Binding) (*Registry, error) {
	r := &Registry{operations: map[Identity]Operation{}, bindings: map[bindingKey]Binding{}}
	for _, op := range operations {
		if op.Identity.ID == "" || op.Identity.Revision == "" {
			return nil, fmt.Errorf("operation identity requires a revision")
		}
		if _, ok := r.operations[op.Identity]; ok {
			return nil, fmt.Errorf("duplicate operation registration")
		}
		r.operations[op.Identity] = op
	}
	for _, b := range bindings {
		if b.Dialect.ID == "" || b.Dialect.Revision == "" {
			return nil, fmt.Errorf("dialect identity requires a revision")
		}
		if _, ok := r.operations[b.Operation]; !ok {
			return nil, fmt.Errorf("unregistered operation")
		}
		key := bindingKey{b.Dialect, b.Operation}
		if _, ok := r.bindings[key]; ok {
			return nil, fmt.Errorf("duplicate dialect/operation binding")
		}
		for _, rule := range b.IdentityRules {
			switch rule.Origin {
			case IdentityBinding, ResourceBinding, TransportOption:
			default:
				return nil, fmt.Errorf("identity rule cannot authorize semantic transformation")
			}
		}
		b.IdentityRules = append([]IdentityRule(nil), b.IdentityRules...)
		r.bindings[key] = b
	}
	return r, nil
}
func (r *Registry) Binding(dialect, operation Identity) (Binding, error) {
	b, ok := r.bindings[bindingKey{dialect, operation}]
	if !ok {
		return Binding{}, fmt.Errorf("unregistered dialect/operation contract")
	}
	b.IdentityRules = append([]IdentityRule(nil), b.IdentityRules...)
	return b, nil
}

func (r *Registry) PrepareIdentity(request Request, destination Descriptor, changes []Change) (Prepared, error) {
	b, err := r.Binding(destination.Dialect, destination.Operation)
	if err != nil {
		return Prepared{}, err
	}
	return prepareIdentity(request, destination, changes, b.IdentityRules)
}
func (r *Registry) Request(d Descriptor, source Document) (Request, error) {
	if _, err := r.Binding(d.Dialect, d.Operation); err != nil {
		return Request{}, err
	}
	return NewRequest(d, source)
}
func (r *Registry) Result(d Descriptor, source Document, outcome Outcome) (Result, error) {
	if _, err := r.Binding(d.Dialect, d.Operation); err != nil {
		return Result{}, err
	}
	return NewResult(d, source, outcome)
}
