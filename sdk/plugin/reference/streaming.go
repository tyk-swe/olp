package main

import "github.com/tyk-swe/olp/sdk/plugin"

// streamingProfile serves OpenAI Responses at the upstream's
// /streaming/v1, which serves only streaming requests. OLP streams every
// request of the profile and aggregates the stream for callers that don't
// stream.
func streamingProfile(api string, headers map[string]string) plugin.Profile {
	return plugin.Profile{
		ID: "reference-streaming", Label: "Reference Responses, streaming only", Dialect: "openai-responses",
		Hosting: plugin.Hosting{Address: api + "/streaming/v1", Headers: headers, ForceStreaming: true},
	}
}
