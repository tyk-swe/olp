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

	MiniMaxProfile  = "minimax-plan"
	MiniMaxUpstream = "https://api.minimax.io"
	MiniMaxKeyPage  = "https://platform.minimax.io/console/plan"

	MiniMaxCNProfile  = "minimax-plan-cn"
	MiniMaxCNUpstream = "https://api.minimax.cn"
	MiniMaxCNKeyPage  = "https://platform.minimax.cn/console/plan"

	KimiProfile  = "kimi-code"
	KimiUpstream = "https://api.kimi.ai/coding"
	KimiKeyPage  = "https://www.kimi.ai/code/console"

	KimiCNProfile  = "kimi-code-cn"
	KimiCNUpstream = "https://api.kimi.com/coding"
	KimiCNKeyPage  = "https://www.kimi.com/code/console"

	QwenTokenPlanProfile  = "qwen-token-plan"
	QwenTokenPlanUpstream = "https://token-plan.ap-southeast-1.maas.aliyuncs.com"
	QwenTokenPlanKeyPage  = "https://modelstudio.console.alibabacloud.com/ap-southeast-1/subscription/overview"

	QwenTokenPlanCNProfile  = "qwen-token-plan-cn"
	QwenTokenPlanCNUpstream = "https://token-plan.cn-beijing.maas.aliyuncs.com"
	QwenTokenPlanCNKeyPage  = "https://bailian.console.aliyun.com/cn-beijing/subscription/overview"
)

// Adapter enrolls the profiles of one vendor's plugin.
type Adapter struct {
	manifest           func() abi.Manifest
	keyPages           map[string]string
	keyPrefix, keyHint string
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

func MiniMax() Adapter {
	return Adapter{manifest: MiniMaxManifest, keyPages: map[string]string{MiniMaxProfile: MiniMaxKeyPage, MiniMaxCNProfile: MiniMaxCNKeyPage},
		keyPrefix: "sk-cp-", keyHint: "Paste the plan's Subscription Key, which starts with sk-cp-, not a pay-as-you-go API key."}
}

func Kimi() Adapter {
	return Adapter{manifest: KimiManifest, keyPages: map[string]string{KimiProfile: KimiKeyPage, KimiCNProfile: KimiCNKeyPage}}
}

func QwenTokenPlan() Adapter {
	return Adapter{manifest: QwenTokenPlanManifest, keyPages: map[string]string{QwenTokenPlanProfile: QwenTokenPlanKeyPage, QwenTokenPlanCNProfile: QwenTokenPlanCNKeyPage},
		keyPrefix: "sk-sp-", keyHint: "Paste the Token Plan key, which starts with sk-sp-, not a pay-as-you-go API key."}
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

func MiniMaxManifest() abi.Manifest {
	return abi.Manifest{
		Name: "minimax-plan", Version: "0.1.0", Description: "MiniMax M Plan API-key enrollment for OLP code-mode. Enrollment does not qualify inference.",
		Origins: []string{"https://api.minimax.io", "https://api.minimax.cn", "https://platform.minimax.io", "https://platform.minimax.cn"},
		Profiles: []abi.Profile{
			profile(MiniMaxProfile, "MiniMax M Plan (code-mode only)", MiniMaxUpstream),
			profile(MiniMaxCNProfile, "MiniMax M Plan, mainland China (code-mode only)", MiniMaxCNUpstream),
		},
	}
}

func KimiManifest() abi.Manifest {
	return abi.Manifest{
		Name: "kimi-code", Version: "0.1.0", Description: "Kimi Code API-key enrollment for OLP code-mode. Enrollment does not qualify inference.",
		Origins: []string{"https://api.kimi.ai", "https://api.kimi.com", "https://www.kimi.ai", "https://www.kimi.com"},
		Profiles: []abi.Profile{
			profile(KimiProfile, "Kimi Code (code-mode only)", KimiUpstream),
			profile(KimiCNProfile, "Kimi Code, mainland China (code-mode only)", KimiCNUpstream),
		},
	}
}

func QwenTokenPlanManifest() abi.Manifest {
	return abi.Manifest{
		Name: "qwen-token-plan", Version: "0.1.0", Description: "Alibaba Model Studio Token Plan Team Edition API-key enrollment for OLP code-mode. Enrollment does not qualify inference.",
		Origins: []string{"https://token-plan.ap-southeast-1.maas.aliyuncs.com", "https://token-plan.cn-beijing.maas.aliyuncs.com", "https://modelstudio.console.alibabacloud.com", "https://bailian.console.aliyun.com"},
		Profiles: []abi.Profile{
			profile(QwenTokenPlanProfile, "Qwen Token Plan Team Edition (code-mode only)", QwenTokenPlanUpstream),
			profile(QwenTokenPlanCNProfile, "Qwen Token Plan Team Edition, mainland China (code-mode only)", QwenTokenPlanCNUpstream),
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
	if a.keyPrefix != "" && !strings.HasPrefix(key, a.keyPrefix) {
		return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: a.keyHint}
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
