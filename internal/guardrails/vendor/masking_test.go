package vendor

import "testing"

func TestNomaSensitiveReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		replacement    bool
	}{
		{"sensitive_only", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}},"promptInjection":{"result":false}}}]}`, true},
		{"another_detector", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}},"promptInjection":{"result":true}}}]}`, false},
		{"another_message", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}}}},{"role":"assistant","results":{"promptInjection":{"result":true}}}]}`, false},
		{"duplicate_message", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}}}},{"role":"user","results":{"promptInjection":{"result":true}}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, reply(tc.response))
			decision, err := evaluate(t, server, "noma", configuration(server.URL, ""), Request{Role: User, Text: "private"})
			if err != nil || !decision.Flagged() || (len(decision.Spans()) > 0) != tc.replacement {
				t.Fatalf("replacement escaped sensitive-only policy: %+v, %v", decision, err)
			}
		})
	}
}
