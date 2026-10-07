package protocols

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Features are the request properties a route selector can test, read from
// the represented semantics of any generation dialect.
type Features struct {
	Tools            bool
	StructuredOutput bool
	// Modalities are the kinds of input parts, in a fixed order: text, image,
	// audio, video and file.
	Modalities []string
	// ReasoningEffort is the effort level the caller named, lowercased.
	ReasoningEffort string
}

var modalities = []string{"text", "image", "audio", "video", "file"}

// RequestFeatures reads the features of a generation request. A request the
// canonical form cannot represent reports what its raw fields show.
func RequestFeatures(r *openai.Request) Features {
	var f Features
	fields := r.Document()
	if c, err := decodeCanonical(r.Family, fields); err == nil {
		f.Tools = len(c.Tools) > 0
		f.StructuredOutput = structured(c.Parameters["response_format"])
		seen := make([]bool, len(modalities))
		for _, message := range c.Messages {
			for _, part := range message.Parts {
				if i := slices.Index(modalities, partModality(part)); i >= 0 {
					seen[i] = true
				}
			}
		}
		for i, name := range modalities {
			if seen[i] {
				f.Modalities = append(f.Modalities, name)
			}
		}
	}
	f.ReasoningEffort = strings.ToLower(reasoningEffort(r.Family.Surface(), fields))
	return f
}

// RequestText is the text a classifier judges: the text parts of the request's
// last user turn, cut to at most limit bytes on a character boundary. It is
// empty when the request has no user text the canonical form can represent.
func RequestText(r *openai.Request, limit int) string {
	c, err := decodeCanonical(r.Family, r.Document())
	if err != nil {
		return ""
	}
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role != "user" {
			continue
		}
		var texts []string
		for _, part := range c.Messages[i].Parts {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		text := strings.Join(texts, "\n")
		if len(text) > limit {
			text = strings.ToValidUTF8(text[:limit], "")
		}
		return text
	}
	return ""
}

func structured(format json.RawMessage) bool {
	fields, err := object(format)
	if !present(format) || err != nil {
		return false
	}
	kind := str(fields["type"])
	return kind == "json_schema" || kind == "json_object"
}

func partModality(p Part) string {
	switch {
	case p.Text != "":
		return "text"
	case strings.HasPrefix(p.MIME, "image/"):
		return "image"
	case strings.HasPrefix(p.MIME, "audio/"):
		return "audio"
	case strings.HasPrefix(p.MIME, "video/"):
		return "video"
	case p.MIME != "":
		return "file"
	case p.URL != "":
		// Image parts may name a URL without a media type.
		return "image"
	}
	return ""
}

// reasoningEffort is the effort a request names in its dialect: OpenAI Chat's
// reasoning_effort and Responses' reasoning.effort, Anthropic's
// output_config.effort, and Gemini's thinkingConfig.thinkingLevel.
func reasoningEffort(surface string, fields Object) string {
	var path []string
	switch surface {
	case "openai":
		if present(fields["reasoning_effort"]) {
			return str(fields["reasoning_effort"])
		}
		path = []string{"reasoning", "effort"}
	case "anthropic":
		path = []string{"output_config", "effort"}
	default:
		path = []string{"generationConfig", "thinkingConfig", "thinkingLevel"}
	}
	value := fields[path[0]]
	for _, key := range path[1:] {
		nested, err := object(value)
		if err != nil {
			return ""
		}
		value = nested[key]
	}
	return str(value)
}
