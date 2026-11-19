package geministate

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/batches/upstream"
)

func TestBatchRepliesNamingAnotherBatchOrInputAreRefusedAndAcceptedOnesShowOnlyCallerFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, nestedName, rootInput string
		valid                       bool
	}{
		{"matching references", "batches/native", "files/native-input", true},
		{"mismatched nested name", "batches/unrelated", "files/native-input", false},
		{"conflicting input alias", "batches/native", "files/unrelated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := fmt.Appendf(nil, `{"name":"batches/native","metadata":{"name":%q,"state":"BATCH_STATE_RUNNING","inputConfig":{"fileName":"files/native-input"}},"input_config":{"file_name":%q}}`, tc.nestedName, tc.rootInput)
			batch, err := upstream.DecodeGeminiBatch(body)
			if err != nil {
				t.Fatalf("could not decode the provider reply: %v", err)
			}
			object, _, err := acceptGeminiBatchResult(body, batch, "gemini-model", geminiInputNames{Local: "files/local-input", Upstream: "files/native-input"})
			if (err == nil) != tc.valid {
				t.Fatalf("acceptance error is %v, want accepted=%v", err, tc.valid)
			}
			if !tc.valid {
				return
			}
			projected, err := object.project("local", "route", nil)
			if err != nil || bytes.Contains(projected, []byte("native-input")) || bytes.Count(projected, []byte("files/local-input")) != 2 {
				t.Fatalf("projection is %s, %v, want both input references naming the caller's file", projected, err)
			}
		})
	}
}
