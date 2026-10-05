package protocols

import (
	"maps"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/vendors"
)

// vendorShape is the reviewed shape of a vendor's requests for an operation,
// with the vendor's name for refusals. A vendor without a contract keeps its
// dialect's shape.
func vendorShape(vendor, operation string) (string, vendors.RequestShape) {
	contract, ok := vendors.Lookup(vendor)
	if !ok {
		return "", vendors.RequestShape{}
	}
	return contract.Name, contract.Request(operation)
}

// validateProfile refuses a request outside the vendor's reviewed contract:
// an operation the vendor does not serve, a field it cannot represent, or
// non-text input where it requires text.
func validateProfile(vendor string, family openai.Family, f Object) error {
	operation := family.Operation()
	if !vendors.Serves(vendor, operation) {
		return requestError("operation", "Operation is outside the configured vendor contract")
	}
	name, shape := vendorShape(vendor, operation)
	for _, field := range shape.Unsupported {
		if present(f[field]) {
			return requestError(field, name+" cannot represent "+field)
		}
	}
	if shape.TextInput {
		if _, ok := textInput(f["input"]); !ok {
			return requestError("input", name+" requires text input")
		}
	}
	return nil
}

// refuseRewriteConflicts refuses a request that sets both names of a field
// the vendor renames, which would otherwise silently lose one value.
func refuseRewriteConflicts(shape vendors.RequestShape, r *openai.Request) error {
	for _, rewrite := range shape.Rewrites {
		if present(r.Field(rewrite.From)) && present(r.Field(rewrite.To)) {
			return requestError(rewrite.From, "Use only one of "+rewrite.From+" and "+rewrite.To)
		}
	}
	return nil
}

// yieldDefaults drops a provider default whose renamed counterpart the request
// sets, so the caller's value survives the rewrite.
func yieldDefaults(shape vendors.RequestShape, r *openai.Request, defaults Object) Object {
	if len(shape.Rewrites) == 0 || len(defaults) == 0 {
		return defaults
	}
	out := maps.Clone(defaults)
	for _, rewrite := range shape.Rewrites {
		if len(r.Field(rewrite.From)) > 0 {
			delete(out, rewrite.To)
		}
		if len(r.Field(rewrite.To)) > 0 {
			delete(out, rewrite.From)
		}
	}
	return out
}

// conform drops the fields the vendor documents as having no effect and
// renames the fields it spells differently.
func conform(shape vendors.RequestShape, f Object) {
	for _, field := range shape.Drop {
		delete(f, field)
	}
	for _, rewrite := range shape.Rewrites {
		value, ok := f[rewrite.From]
		if !ok {
			continue
		}
		if rewrite.Value != nil {
			value = rewrite.Value
		}
		f[rewrite.To] = value
		delete(f, rewrite.From)
	}
}
