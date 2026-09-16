package protocols

import (
	"bytes"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// MeaningfulFrame distinguishes model output from protocol setup, heartbeats,
// and usage-only events. Performance measurements use delivered output only.
func MeaningfulFrame(family openai.Family, frame []byte) bool {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		f, e := object(bytes.TrimSpace(line[5:]))
		if e != nil {
			continue
		}
		switch family.Surface() {
		case "anthropic":
			delta, _ := object(f["delta"])
			block, _ := object(f["content_block"])
			if str(delta["text"]) != "" || str(delta["partial_json"]) != "" || str(block["text"]) != "" || str(block["type"]) == "tool_use" {
				return true
			}
		case "gemini":
			for _, v := range arr(f["candidates"]) {
				candidate, _ := object(v)
				content, _ := object(candidate["content"])
				for _, part := range arr(content["parts"]) {
					p, _ := object(part)
					if string(p["thought"]) != "true" && (str(p["text"]) != "" || present(p["functionCall"])) {
						return true
					}
				}
			}
		default:
			if family == openai.FamilyResponses {
				if str(f["delta"]) != "" {
					return true
				}
				item, _ := object(f["item"])
				if str(item["type"]) == "function_call" {
					return true
				}
			} else {
				for _, v := range arr(f["choices"]) {
					choice, _ := object(v)
					delta, _ := object(choice["delta"])
					if str(delta["content"]) != "" || str(delta["refusal"]) != "" || len(arr(delta["tool_calls"])) > 0 {
						return true
					}
				}
			}
		}
	}
	return false
}
