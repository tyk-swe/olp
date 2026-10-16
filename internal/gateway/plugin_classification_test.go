package gateway

import (
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// declarePlugin makes provider a a plugin provider whose profile declares the
// classification.
func (h *harness) declarePlugin(classification ...abi.FailureRule) {
	h.t.Helper()
	h.servePluginProfile(abi.Hosting{Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}, Classification: classification})
}
