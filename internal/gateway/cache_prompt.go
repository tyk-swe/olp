package gateway

import "encoding/json"

func (x *call) promptCacheKey() string {
	if x.route == nil || x.route.PromptCache == nil {
		return ""
	}
	session := ""
	if x.route.PromptCache.KeySource == "session" {
		session = x.attribution[x.route.PromptCache.SessionLabel]
		if session == "" {
			return ""
		}
	}
	raw, _ := json.Marshal([]string{"olp-provider-prompt-cache-v1", x.captureProject(), x.keyID, session})
	return hexDigest(raw)
}
