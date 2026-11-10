package vendor

import (
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/upstream"
)

type Rejection struct {
	Class    upstream.Class
	Envelope string
	RetryAt  time.Time
}

func (v Vendor) InitialRejection(endpoint Endpoint, status int, body []byte, headers http.Header, now time.Time) (Rejection, bool) {
	if status == 429 {
		classification, ok := v.Classify(endpoint, status, body, headers, now)
		if !ok || !classification.NotExecuted {
			return Rejection{}, false
		}
		return Rejection{Class: upstream.RateLimit, Envelope: classification.Envelope, RetryAt: classification.RetryAt}, true
	}
	if v.Adapter != code.AdapterCodex || endpoint.Observation != ObserveCodex || status != 401 {
		return Rejection{}, false
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object || len(document.Root().Members()) != 1 {
		return Rejection{}, false
	}
	errorType, _ := document.Lookup("/error/type")
	errorCode, _ := document.Lookup("/error/code")
	typeName, _ := errorType.Text()
	errCode, _ := errorCode.Text()
	if typeName != "authentication_error" || errCode != "invalid_api_key" {
		return Rejection{}, false
	}
	outcome := (upstream.Classifier{AtMostOnce: true}).Classify(upstream.Evidence{Reached: true, Status: status, Error: &wire.UpstreamError{Type: typeName, Code: errCode}})
	if outcome.Acceptance != upstream.Terminal || outcome.Class != upstream.Credential {
		return Rejection{}, false
	}
	return Rejection{Class: outcome.Class, Envelope: "codex_invalid_api_key", RetryAt: rejectionRetryAt(document, headers, now)}, true
}
