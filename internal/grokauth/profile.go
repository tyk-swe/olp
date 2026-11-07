package grokauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"github.com/tyk-swe/olp/plugin/abi"
)

const (
	ProfileID            = "grok-build-subscription"
	Issuer               = "https://auth.x.ai"
	Upstream             = "https://cli-chat-proxy.grok.com/v1"
	CompatibilityVersion = "1.0.45"
	ClientID             = "b1a00492-073a-47ea-816f-4c329264a828"
)

func Manifest() abi.Manifest {
	return abi.Manifest{Name: "grok-build", Version: "0.1.0", Description: "Grok Build device enrollment for code mode. Live provider acceptance is unverified.", Origins: []string{Issuer, "https://cli-chat-proxy.grok.com"}, Profiles: []abi.Profile{{ID: ProfileID, Label: "Grok Build subscription (code-mode only)", Dialect: "openai-responses", Signing: true, Grant: &abi.GrantAuthentication{Facts: []string{"subject"}}, Hosting: abi.Hosting{Address: Upstream, Headers: map[string]string{"Authorization": "Bearer {credential}"}}}}}
}

func (Adapter) Sign(context.Context, abi.SignRequest) (abi.SignResult, error) {
	return abi.SignResult{}, &abi.Error{Code: "code_mode_required", Message: "Use a published code-mode route. Ordinary inference and probes are disabled."}
}

func Principal(subject string) string {
	if subject == "" || len(subject) > 1024 || strings.TrimSpace(subject) != subject || strings.ContainsFunc(subject, unicode.IsControl) {
		return ""
	}
	digest := sha256.Sum256([]byte(Issuer + "\x00" + subject))
	return "grok:" + hex.EncodeToString(digest[:])
}
