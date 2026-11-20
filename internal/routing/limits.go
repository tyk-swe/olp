package routing

// Configured returns nil for a quota that declares no bounds.
func (l *Limits) Configured() *Limits {
	if l == nil || (l.RequestsPerMinute == nil && !l.TokenLimited() && l.MaxConcurrency == nil && l.Costs() == nil) {
		return nil
	}
	return l
}

func (l *Limits) TokenLimited() bool {
	return l != nil && (l.TokensPerMinute != nil || l.InputTokensPerMinute != nil || l.OutputTokensPerMinute != nil)
}

func (s *Slot) TokenLimited() bool {
	return s != nil && (s.TokensPerMinute != nil || s.InputTokensPerMinute != nil || s.OutputTokensPerMinute != nil)
}

func (s Slot) limits() Limits {
	return Limits{
		RequestsPerMinute: s.RequestsPerMinute, TokensPerMinute: s.TokensPerMinute,
		InputTokensPerMinute: s.InputTokensPerMinute, OutputTokensPerMinute: s.OutputTokensPerMinute,
		MaxConcurrency: s.MaxConcurrency, Supply: s.Supply,
	}
}
