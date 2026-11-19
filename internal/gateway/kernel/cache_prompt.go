package kernel

import "encoding/json"

func (x *Call) promptCacheKey() string {
	if x.Route == nil || x.Route.PromptCache == nil {
		return ""
	}
	session := ""
	if x.Route.PromptCache.KeySource == "session" {
		session = x.attribution[x.Route.PromptCache.SessionLabel]
		if session == "" {
			return ""
		}
	}
	raw, _ := json.Marshal([]string{"olp-provider-prompt-cache-v1", x.captureProject(), x.keyID, session})
	return hexDigest(raw)
}
