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
	}
	return nil
}
