package gateway

import (
	"crypto/sha256"
	"encoding/base32"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codeValidateAntigravityMessages(request *wire.Request, r *http.Request) error {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 0 && (len(query) != 1 || !slices.Equal(query["beta"], []string{"true"})) || len(r.Trailer) != 0 || len(r.Header.Values("Anthropic-Beta")) != 0 || len(r.Header.Values("X-Codex-Turn-State")) != 0 || len(r.Header.Values("OpenAI-Beta")) != 0 {
		return refused
	}
	for _, field := range []string{"stream_options", "thinking", "output_config"} {
		if request.Field(field) != nil {
			return refused
		}
	}
	root := request.OIF().Document().Root()
	if !codeTextContent(root, "system") {
		return refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !codeMessagesContent(message) {
			return refused
		}
		content, _ := message.Lookup("content")
		for _, block := range content.Elements() {
			if value, present := block.Lookup("is_error"); present && value.Raw() != "false" {
				return refused
			}
		}
	}
	return nil
}

func codeAntigravityMessagesIdentity(request *wire.Request, enabled bool) (string, error) {
	metadata, present := request.OIF().Document().Root().Lookup("metadata")
	if !present {
		return "", nil
	}
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if !enabled || metadata.Kind() != oif.Object || len(metadata.Members()) != 1 {
		return "", refused
	}
	user, _ := metadata.Lookup("user_id")
	identity, text := user.Text()
	if !text || identity == "" {
		return "", refused
	}
	digest := sha256.Sum256([]byte(identity))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])), nil
}
