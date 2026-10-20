package codeclient

import (
	"slices"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func qualificationGaps(adapters []codemode.Adapter, client string) []string {
	var gaps []string
	for _, adapter := range adapters {
		vendor, _ := codeadapter.Lookup(adapter)
		for _, note := range vendor.EnrollmentNotes() {
			if !slices.Contains(gaps, note) {
				gaps = append(gaps, note)
			}
		}
	}
	for _, adapter := range adapters {
		vendor, _ := codeadapter.Lookup(adapter)
		gaps = append(gaps, vendor.Notes()...)
	}
	if len(adapters) > 1 {
		gaps = append(gaps,
			"Each request carries the conversation so far, so every subscription a conversation mixes receives the turns the others produced.",
			"No live vendor is qualified to accept another vendor's reasoning and tool calls in that history; mixed conversations are qualified against scripted vendors only.")
	}
	switch client {
	case codeadapter.ClientClaudeCode:
		gaps = append(gaps,
			"Token counting and the connectivity probe are not served; Claude Code falls back to local estimates.",
			"Server tools such as web search and web fetch, and claude.ai login features, are outside code mode.",
			"Each generation is limited to ten minutes; raising API_TIMEOUT_MS cannot extend it.")
	case codeadapter.ClientOpenCode:
		gaps = append(gaps,
			"OpenCode picks each model's endpoint from its own model catalogue; a route model the installed release does not know is unavailable.",
			"Chat Completions usage settles only from the final usage chunk; a stream without it leaves consumption uncertain.")
	}
	for _, adapter := range adapters {
		vendor, _ := codeadapter.Lookup(adapter)
		gaps = append(gaps, vendor.ClientNotes(client)...)
	}
	return gaps
}
