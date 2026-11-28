package geminilifecycle

import (
	"encoding/json"
	"testing"
)

func TestInteractionRequestReportsItsReplyBound(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		config string
		want   int64
	}{
		"bounded":      {config: `,"generation_config":{"max_output_tokens":256}`, want: 256},
		"no config":    {},
		"no bound":     {config: `,"generation_config":{"temperature":0.2}`},
		"not positive": {config: `,"generation_config":{"max_output_tokens":0}`},
		"fractional":   {config: `,"generation_config":{"max_output_tokens":1.5}`},
		"not a number": {config: `,"generation_config":{"max_output_tokens":"256"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := []byte(`{"model":"route","input":"hello"` + tc.config + `}`)
			request, err := ParseInteractionRequest(body, len(body))
			if err != nil {
				t.Fatal(err)
			}
			if request.MaxOutputTokens != tc.want {
				t.Fatalf("MaxOutputTokens = %d, want %d", request.MaxOutputTokens, tc.want)
			}
		})
	}
}

func TestLiveServerFrameReportsOnlyTrustworthyUsage(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ usage, want string }{
		"response counts": {`{"promptTokenCount":10,"responseTokenCount":5,"candidatesTokenCount":9,"cachedContentTokenCount":10}`,
			`{"input":10,"output":5}`},
		"candidates stand in for the response": {`{"promptTokenCount":10,"candidatesTokenCount":5,"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":5}]}`,
			`{"input":10,"output":5,"details":[{"modality":"AUDIO","tokenCount":5}]}`},
		"input missing":       {`{"responseTokenCount":5}`, `{}`},
		"output missing":      {`{"promptTokenCount":10}`, `{}`},
		"negative thoughts":   {`{"promptTokenCount":10,"responseTokenCount":5,"thoughtsTokenCount":-1}`, `{}`},
		"negative detail":     {`{"promptTokenCount":10,"responseTokenCount":5,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":-1}]}`, `{}`},
		"cached above input":  {`{"promptTokenCount":10,"responseTokenCount":5,"cachedContentTokenCount":11}`, `{}`},
		"fractional count":    {`{"promptTokenCount":10.5,"responseTokenCount":5}`, `{}`},
		"count sent as text":  {`{"promptTokenCount":"10","responseTokenCount":5}`, `{}`},
		"zero counts are set": {`{"promptTokenCount":0,"responseTokenCount":0}`, `{"input":0,"output":0}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			frame, err := ObserveLiveServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":`+tc.usage+`}`), 1<<16)
			if err != nil || !frame.TurnComplete {
				t.Fatalf("frame = %+v, %v", frame, err)
			}
			var observed struct {
				Input   *int64            `json:"input,omitempty"`
				Output  *int64            `json:"output,omitempty"`
				Details []LiveUsageDetail `json:"details,omitempty"`
			}
			if u := frame.Usage; u != nil {
				observed.Input, observed.Output, observed.Details = u.Input, u.Output, u.ResponseDetails
			}
			encoded, err := json.Marshal(observed)
			if err != nil || string(encoded) != tc.want {
				t.Fatalf("usage = %s, want %s, %v", encoded, tc.want, err)
			}
		})
	}
}
