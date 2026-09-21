package gateway

import (
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"strings"
)

var contextWindowCodes = map[string]bool{
	"contextlengthexceeded":    true,
	"contextwindowexceeded":    true,
	"maxcontextlengthexceeded": true,
	"prompttoolong":            true,
}

var contextCodeFold = strings.NewReplacer("_", "", "-", "", " ", "")

func contextWindowError(e *openai.UpstreamError) bool {
	if e == nil {
		return false
	}
	return contextWindowCodes[contextCodeFold.Replace(strings.ToLower(e.Code))] ||
		contextWindowCodes[contextCodeFold.Replace(strings.ToLower(e.Type))]
}

// Streaming providers report failures inside a successful HTTP response.
// Classify their typed envelopes at the same attempt boundary as HTTP errors.
func inBandFailure(e *openai.UpstreamError, committed bool) (string, int) {
	kind := contextCodeFold.Replace(strings.ToLower(e.Type + " " + e.Code))
	has := func(names ...string) bool {
		for _, name := range names {
			if strings.Contains(kind, name) {
				return true
			}
		}
		return false
	}
	switch {
	case contextWindowError(e):
		return classContextWindow, 400
	case has("authentication", "unauthenticated", "invalidapikey", "accessdenied", "permissiondenied") || e.Code == "401" || e.Code == "403":
		return classCredential, 401
	case has("ratelimit", "resourceexhausted", "throttl") || e.Code == "429":
		return classRateLimit, 429
	case has("timeout", "deadlineexceeded") || e.Code == "504":
		return classTimeout, 504
	case has("invalidrequest", "invalidargument", "validationexception", "notfound") || e.Code == "400" || e.Code == "404":
		return classUpstreamClient, 400
	}
	if committed {
		return classProtocol, 502
	}
	return classUpstreamServer, 502
}
