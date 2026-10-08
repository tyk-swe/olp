// Package bodylimit defines the operator's optional per-route ingress bound.
package bodylimit

// Maximum matches the largest supported installation media-body limit.
const Maximum int64 = 1 << 30

// Valid accepts an inherited limit or a positive supported byte count.
func Valid(value *int64) bool { return value == nil || *value > 0 && *value <= Maximum }

// Lower preserves the installation or protocol cap when the route is unset or
// more permissive. Route validation rejects nonpositive values.
func Lower(installation int64, route *int64) int64 {
	if route != nil && *route > 0 {
		return min(installation, *route)
	}
	return installation
}
