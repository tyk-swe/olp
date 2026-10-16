//go:build integration

package gateway

import ()

type grantGenerationRuntime struct {
	*fakeRuntime
	generation int64
}

func (r *grantGenerationRuntime) GrantGeneration(string) int64 { return r.generation }

// A refusal of the replacement is effective, and a delayed refresh
// notification only clears the superseded generation's cooldown.
