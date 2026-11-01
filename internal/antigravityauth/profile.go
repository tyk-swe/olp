package antigravityauth

import (
	"context"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const (
	ProfileID        = "antigravity-subscription"
	Issuer           = "https://accounts.google.com"
	TokenEndpoint    = "https://oauth2.googleapis.com/token"
	UserInfoEndpoint = "https://openidconnect.googleapis.com/v1/userinfo"
	Upstream         = "https://daily-cloudcode-pa.googleapis.com"
	ProjectEndpoint  = Upstream + "/v1internal:loadCodeAssist"
	RedirectURI      = "http://127.0.0.1:51121/oauth-callback"
)

func Manifest() abi.Manifest {
	return abi.Manifest{
		Name: "antigravity", Version: "0.1.0", Description: "Headless Antigravity enrollment. Controlled protocol support does not qualify live subscription inference.",
		Origins: []string{Issuer, "https://oauth2.googleapis.com", "https://openidconnect.googleapis.com", Upstream},
		Profiles: []abi.Profile{{
			ID: ProfileID, Label: "Antigravity subscription (code-mode only)", Dialect: "gemini-generate-content", Signing: true,
			Options: []abi.Option{{Name: "client_id", Label: "OAuth client ID", Description: "The operator supplies an authorized Google desktop OAuth client. Paste its secret only when completing enrollment.", Pattern: `^[a-zA-Z0-9._-]+\.apps\.googleusercontent\.com$`}},
			Grant:   &abi.GrantAuthentication{Input: abi.GrantInputSecret, Facts: []string{"subject", "project_id"}},
			Hosting: abi.Hosting{Address: Upstream, Headers: map[string]string{"Authorization": "Bearer {credential}"}},
		}},
	}
}

func (Adapter) Manifest() abi.Manifest { return Manifest() }

func (Adapter) Sign(context.Context, abi.SignRequest) (abi.SignResult, error) {
	return abi.SignResult{}, &abi.Error{Code: "code_mode_required", Message: "Use a published code-mode route. Ordinary inference and synthetic probes are disabled."}
}

func Principal(subject, project string) string {
	if !identifier(subject) || !identifier(project) {
		return ""
	}
	return Issuer + "/" + url.PathEscape(subject) + "/" + url.PathEscape(project)
}

func identifier(value string) bool {
	return len(value) > 0 && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00/\\") && strings.TrimSpace(value) == value
}
