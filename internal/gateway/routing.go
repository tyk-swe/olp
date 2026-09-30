package gateway

import "github.com/tyk-swe/olp/internal/runtime"

// selectionOptions supplies the shared routing authority and measurements.
// Each operation adds its own source demand and semantic preparation.
func (s *Server) selectionOptions(x *execution) runtime.SelectionOptions {
	return runtime.SelectionOptions{
		KeyID: x.keyID, Preferences: x.preferences,
		Inputs: s.Runtime.RoutingInputs(), Now: s.now(),
		CheckSlots: true, CredentialEligibility: s.Runtime.Eligibility,
		UnconfinedPlugins: s.cfg.UnconfinedPlugins,
	}
}
