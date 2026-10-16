package routing

import "errors"

func (r Route) ValidateCaching() error {
	if err := r.Cache.Validate(); err != nil {
		return err
	}
	if err := r.PromptCache.Validate(); err != nil {
		return err
	}
	if r.Fidelity.Strict() && (r.PromptCache != nil || r.Cache != nil && r.Cache.Mode == "semantic") {
		return errors.New("semantic caching and prompt-cache insertion require a transformed route")
	}
	return nil
}
