package runtime

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/workload"
)

func (m *Manager) VerifyEntraUser(ctx context.Context, raw string, expected access.Authority, issuer, audience string) (workload.UserAssertion, error) {
	if ctx.Err() != nil || expected.WorkloadIssuerID == nil || expected.EndUserDigest == "" {
		return workload.UserAssertion{}, ErrInvalidKey
	}
	current, err := m.Authenticate(raw)
	if err != nil || current.WorkloadIssuerID == nil || *current.WorkloadIssuerID != *expected.WorkloadIssuerID || current.ID != expected.ID || current.EndUserDigest != expected.EndUserDigest || current.WorkloadRevision != expected.WorkloadRevision {
		return workload.UserAssertion{}, ErrInvalidKey
	}
	token, parseErr := workload.Parse(raw)
	if parseErr != nil {
		return workload.UserAssertion{}, ErrInvalidKey
	}
	return token.EntraUser(issuer, audience)
}
