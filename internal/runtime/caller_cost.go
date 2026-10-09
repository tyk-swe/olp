package runtime

import "fmt"

// Exempt requests may only spend caller credentials. Route transitions preserve
// the admission policy; classifier requests have their own independently admitted route.
func (s *Snapshot) validateCallerCostPolicy(route Route) error {
	for _, target := range route.Targets {
		caller := s.Providers[target.ProviderID].CredentialSource == "caller"
		if target.Shadow != nil {
			if caller {
				return fmt.Errorf("route %q shadow target requires operator credentials", route.Slug)
			}
			continue
		}
		if route.CallerCostExempt && !caller {
			return fmt.Errorf("route %q exempts a target that uses operator credentials", route.Slug)
		}
	}
	check := func(slug string) error {
		if next, ok := s.Routes[slug]; ok && next.CallerCostExempt != route.CallerCostExempt {
			return fmt.Errorf("route %q changes caller cost policy through %q", route.Slug, slug)
		}
		return nil
	}
	for _, fallback := range route.Fallbacks {
		if err := check(fallback.Route); err != nil {
			return err
		}
	}
	for _, selector := range route.Selectors {
		if selector.Route != "" {
			if err := check(selector.Route); err != nil {
				return err
			}
		}
		if c := selector.When.Classifier; c != nil && s.servesWithCallerCredentials(c.Route, map[string]bool{}) {
			return fmt.Errorf("route %q classifies with %q, which requires caller credentials", route.Slug, c.Route)
		}
	}
	return nil
}

// servesWithCallerCredentials reports whether a route, or a route it falls back
// or delegates to, spends caller credentials. A classifier request carries no
// caller credential, so such a route could never answer it.
func (s *Snapshot) servesWithCallerCredentials(slug string, seen map[string]bool) bool {
	route, ok := s.Routes[slug]
	if !ok || seen[slug] {
		return false
	}
	seen[slug] = true
	for _, target := range route.Targets {
		if target.Shadow == nil && s.Providers[target.ProviderID].CredentialSource == "caller" {
			return true
		}
	}
	for _, edge := range route.edges() {
		if edge.serving && s.servesWithCallerCredentials(edge.to, seen) {
			return true
		}
	}
	return false
}
