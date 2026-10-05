// Package codeplans implements enrollment for API-key coding plans used by
// OLP code-mode. The operator opens the vendor's key page and pastes the key it
// issues. The key becomes the grant's access token; it neither expires nor
// refreshes, and a fingerprint of it is the observed principal. Signing
// refuses, so only the code-mode authorizer places the key on a request.
package codeplans

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const (
	OpenCodeGoProfile  = "opencode-go"
	OpenCodeGoUpstream = "https://opencode.ai/zen/go/v1"
	OpenCodeGoKeyPage  = "https://opencode.ai/auth"

	ZAIProfile  = "zai-coding-plan"
	ZAIUpstream = "https://api.z.ai/api"
	ZAIKeyPage  = "https://z.ai/manage-apikey/apikey-list"

	BigModelProfile  = "bigmodel-coding-plan"
	BigModelUpstream = "https://open.bigmodel.cn/api"
	BigModelKeyPage  = "https://open.bigmodel.cn/usercenter/apikeys"
)

// Adapter enrolls the profiles of one vendor's plugin.
type Adapter struct {
	manifest func() abi.Manifest
	keyPages map[string]string
}

// OpenCodeGo is the OpenCode Go plugin's adapter.
func OpenCodeGo() Adapter {
	return Adapter{manifest: OpenCodeGoManifest, keyPages: map[string]string{OpenCodeGoProfile: OpenCodeGoKeyPage}}
}

// ZAI is the Z.ai GLM Coding Plan plugin's adapter, for the global and the
// mainland China (BigModel) account systems.
func ZAI() Adapter {
	return Adapter{manifest: ZAIManifest, keyPages: map[string]string{ZAIProfile: ZAIKeyPage, BigModelProfile: BigModelKeyPage}}
}

// OpenCodeGoManifest declares the OpenCode Go profile. Its dialect is nominal:
// code-mode forwards client bytes, and Sign fences every other use.
func OpenCodeGoManifest() abi.Manifest {
	return abi.Manifest{
		Name: "opencode-go", Version: "0.1.0", Description: "OpenCode Go API-key enrollment for OLP code-mode. Enrollment does not qualify inference.",
		Origins:  []string{"https://opencode.ai"},
		Profiles: []abi.Profile{profile(OpenCodeGoProfile, "OpenCode Go subscription (code-mode only)", OpenCodeGoUpstream)},
	}
}

// ZAIManifest declares the GLM Coding Plan profiles of Z.ai and BigModel.
func ZAIManifest() abi.Manifest {
	return abi.Manifest{
		Name: "zai-coding", Version: "0.1.0", Description: "Z.ai GLM Coding Plan API-key enrollment for OLP code-mode. Enrollment does not qualify inference.",
		Origins: []string{"https://api.z.ai", "https://open.bigmodel.cn", "https://z.ai"},
		Profiles: []abi.Profile{
			profile(ZAIProfile, "GLM Coding Plan, Z.ai (code-mode only)", ZAIUpstream),
			profile(BigModelProfile, "GLM Coding Plan, BigModel (code-mode only)", BigModelUpstream),
		},
	}
}

func profile(id, label, address string) abi.Profile {
	return abi.Profile{
		ID: id, Label: label, Dialect: "openai-chat", Signing: true,
		Grant:   &abi.GrantAuthentication{Input: abi.GrantInputSecret},
		Hosting: abi.Hosting{Address: address, Headers: map[string]string{"Authorization": "Bearer {credential}"}},
	}
}

func (a Adapter) Manifest() abi.Manifest { return a.manifest() }

type session struct {
	Profile string `json:"profile"`
}

// StartGrant sends the operator to the vendor page that issues the key.
func (a Adapter) StartGrant(_ context.Context, start abi.GrantStart) (abi.GrantAuthorization, error) {
	page, ok := a.keyPages[start.Profile]
	if !ok {
		return abi.GrantAuthorization{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "This plugin has no such profile."}
	}
	state, err := json.Marshal(session{Profile: start.Profile})
	return abi.GrantAuthorization{URL: page, Session: string(state)}, err
}

// ExchangeGrant accepts the pasted key. It makes no upstream call, so
// enrollment performs no inference and proves no entitlement.
func (a Adapter) ExchangeGrant(_ context.Context, exchange abi.GrantExchange) (abi.Grant, error) {
	var s session
	if json.Unmarshal([]byte(exchange.Session), &s) != nil || s.Profile != exchange.Profile || a.keyPages[s.Profile] == "" {
		return abi.Grant{}, &abi.Error{Code: abi.CodeStateMismatch, Message: "This enrollment belongs to another profile. Start enrollment again."}
	}
	key := strings.TrimSpace(exchange.Input)
	if !ValidKey(key) {
		return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "Paste the API key the vendor's key page issued, not an OLP key or a URL."}
	}
	return abi.Grant{AccessToken: key, Principal: Principal(exchange.Profile, key)}, nil
}

// Sign fences ordinary provider probes and transformed or strict inference.
// Only the code-mode authorizer may place these keys on an inference request.
func (Adapter) Sign(context.Context, abi.SignRequest) (abi.SignResult, error) {
	return abi.SignResult{}, &abi.Error{Code: "code_mode_required", Message: "Use a published code-mode route; ordinary inference and synthetic probes are disabled."}
}

// Principal identifies the enrolled key within its profile's account system
// without revealing it. A different key is a different principal.
func Principal(profileID, key string) string {
	sum := sha256.Sum256([]byte("olp-code-key\x00" + profileID + "\x00" + key))
	return profileID + ":" + hex.EncodeToString(sum[:])
}

// ValidKey reports whether key could be a vendor API key: 16–512 printable
// ASCII characters, neither an OLP key nor a URL.
func ValidKey(key string) bool {
	if len(key) < 16 || len(key) > 512 || strings.HasPrefix(key, "olp_") || strings.Contains(key, "://") {
		return false
	}
	for i := range len(key) {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}
