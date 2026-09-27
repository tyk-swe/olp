package providers

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestKindAuthModesAgreeWithTheirAuthenticators(t *testing.T) {
	for _, kind := range kinds {
		for _, mode := range kind.AuthModes {
			if connectors.SecretRequired(mode.Mode) != (mode.Credential == "required") {
				t.Errorf("%s %s: catalog credential %q disagrees with its authenticator", kind.Kind, mode.Mode, mode.Credential)
			}
		}
	}
	cfg := Configuration{Kind: KindOpenAI, AuthMode: "unregistered"}
	cfg.Normalize()
	if problem, ok := errors.AsType[*access.Problem](cfg.Validate(&egress.Policy{})); !ok || problem.Field != "configuration.auth_mode" {
		t.Fatal("an unknown auth mode passed validation")
	}
}
