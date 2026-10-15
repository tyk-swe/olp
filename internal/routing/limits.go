package routing

// Configured returns nil for a quota that declares no bounds.
func (l *Limits) Configured() *Limits {
	if l == nil || (l.RequestsPerMinute == nil && l.TokensPerMinute == nil && l.MaxConcurrency == nil && l.Costs() == nil) {
		return nil
	}
	return l
}
