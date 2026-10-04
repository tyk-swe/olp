// Package codexauth implements the Codex device authorization contract.
package codexauth

import (
	"context"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const (
	ClientVersion  = "0.160.0"
	SourceRevision = "a956835d020762cb2b570053af06f643a11c0ecc"
	ProfileID      = "codex-subscription"
	Upstream       = "https://chatgpt.com/backend-api/codex"
	Issuer         = "https://auth.openai.com"
	ClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
)

func Manifest() abi.Manifest {
	return abi.Manifest{
		Name: "codex", Version: "0.1.0", Description: "Codex device enrollment for OLP code-mode. Enrollment does not qualify inference.",
		Origins: []string{Issuer, "https://chatgpt.com"},
		Profiles: []abi.Profile{{
			ID: ProfileID, Label: "Codex subscription (code-mode only)", Dialect: "openai-responses", Signing: true,
			Grant: &abi.GrantAuthentication{Facts: []string{"account_id", "user_id"}},
			Hosting: abi.Hosting{Address: Upstream, Headers: map[string]string{
				"Authorization": "Bearer {credential}", "ChatGPT-Account-ID": "{grant.account_id}",
			}},
		}},
	}
}

// Sign fences ordinary provider probes and transformed/strict inference. Only
// the code-mode authorizer may place these credentials on an inference request.
func (Adapter) Sign(context.Context, abi.SignRequest) (abi.SignResult, error) {
	return abi.SignResult{}, &abi.Error{Code: "code_mode_required", Message: "Use a published code-mode route; ordinary inference and synthetic probes are disabled."}
}
