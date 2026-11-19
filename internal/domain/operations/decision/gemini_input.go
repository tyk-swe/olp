package decision

import (
	"mime"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

func geminiBlocks(value oif.Value) []oif.Value {
	if value.Kind() == oif.Array {
		return value.Elements()
	}
	return []oif.Value{value}
}

func validateGeminiContent(value oif.Value, field string, allowMedia bool) error {
	if text(value) != "" {
		return nil
	}
	blocks := geminiBlocks(value)
	for _, block := range blocks {
		kind := text(member(block, "type"))
		if kind == "text" && known(block, "type text") == nil && text(member(block, "text")) != "" {
			continue
		}
		if allowMedia && validGeminiMedia(block, kind) {
			continue
		}
		if allowMedia {
			return operations.Invalid(field, "Use text or image, audio, video, or PDF blocks with inline data or an HTTPS URI.")
		}
		return operations.Invalid(field, "Use text or text blocks for question instructions.")
	}
	if len(blocks) == 0 {
		return operations.Invalid(field, "Provide non-empty content.")
	}
	return nil
}

func validGeminiMedia(block oif.Value, kind string) bool {
	const fields = "type mime_type data uri"
	mediaFields := map[string]string{
		"audio":    fields + " channels sample_rate processing",
		"document": fields + " resolution processing",
		"image":    fields + " resolution",
		"video":    fields + " resolution processing name",
	}
	names, ok := mediaFields[kind]
	if !ok || known(block, names) != nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(text(member(block, "mime_type")))
	if err != nil || (kind == "document" && mediaType != "application/pdf") ||
		(kind != "document" && !strings.HasPrefix(mediaType, kind+"/")) {
		return false
	}
	data, uri := member(block, "data"), member(block, "uri")
	if operations.Optional(uri) {
		return text(data) != ""
	}
	if !operations.Optional(data) {
		return false
	}
	parsed, err := url.ParseRequestURI(text(uri))
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil
}

func geminiFetchesInput(request oif.Request) bool {
	return slices.ContainsFunc(geminiBlocks(member(request.Document().Root(), "input")), func(block oif.Value) bool {
		return !operations.Optional(member(block, "uri"))
	})
}

func geminiJoinedText(value oif.Value) string {
	if value.Kind() == oif.String {
		return text(value)
	}
	return strings.Join(slices.Collect(func(yield func(string) bool) {
		for _, block := range geminiBlocks(value) {
			if !yield(text(member(block, "text"))) {
				return
			}
		}
	}), "")
}

func geminiInputText(request oif.Request) ([]operations.Text, error) {
	root := request.Document().Root()
	input := member(root, "input")
	if err := validateGeminiContent(input, "/input", false); err != nil {
		return nil, operations.Error("policy_conflict", "/input", "inspectable_input", "Text policies cannot inspect Gemini Decisions media.")
	}
	return slices.Collect(func(yield func(operations.Text) bool) {
		if !yield(operations.Text{Pointer: "/input", Value: geminiJoinedText(input)}) {
			return
		}
		for _, question := range member(root, "questions").Members() {
			at := operations.Pointer("/questions", question.Name)
			if !yield(operations.Text{Pointer: at, Value: question.Name}) ||
				!yield(operations.Text{Pointer: at + "/instructions", Value: geminiJoinedText(member(question.Value, "instructions"))}) {
				return
			}
			for _, criterion := range collectText(member(question.Value, "criteria"), at+"/criteria", true) {
				if !yield(criterion) {
					return
				}
			}
		}
	}), nil
}
