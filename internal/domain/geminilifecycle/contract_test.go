package geminilifecycle

import "testing"

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
