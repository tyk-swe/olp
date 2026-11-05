package codeallowance

import (
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
)

const MiniMaxRemainsURL = "https://www.minimax.io/v1/token_plan/remains"

var ErrMiniMaxRemainsUnqualified = errors.New("minimax remains schema is not pinned from real keys")

func MiniMaxRemainsQualified(profileID string) bool {
	if profileID != codeplans.MiniMaxProfile && profileID != codeplans.MiniMaxCNProfile {
		return false
	}
	signal, ok := codeadapter.AllowanceSignalForProfile("plugin", "grant", profileID)
	return ok && signal.QualifiedForActiveRead()
}

func ParseMiniMaxRemains(_ []byte, _ time.Time) (codemode.Allowance, error) {
	return codemode.Allowance{}, ErrMiniMaxRemainsUnqualified
}
