package gateway

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type codePayload struct {
	summaryCompaction  bool
	signedContinuation bool
	body               []byte
	query              string
	stream             bool
	translated         bool
	includeUsage       bool
	target             codeadapter.Target
}

func codeParseMessages(body []byte, r *http.Request, model string, safetyMapping bool) (*wire.Request, string, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil || (len(query) != 0 && (len(query) != 1 || !slices.Equal(query["beta"], []string{"true"}))) || r.Header.Get("Anthropic-Beta") != "" || len(r.Trailer) != 0 {
		return nil, "", refused
	}
	request, requestErr := protocols.ParseForModel(wire.Anthropic, body, model)
	if requestErr != nil {
		return nil, "", refused
	}
	if request.Field("stream_options") != nil {
		return nil, "", refused
	}
	root := request.OIF().Document().Root()
	if !codeTextContent(root, "system") {
		return nil, "", refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !codeMessagesContent(message) {
			return nil, "", refused
		}
	}
	prepared, safety, identityErr := codeMessagesSafetyIdentifier(request, safetyMapping)
	if identityErr != nil {
		return nil, "", identityErr
	}
	return prepared, safety, nil
}

func codeTranslateMessages(body []byte, r *http.Request, model string, minimum, safetyMapping bool) (codePayload, error) {
	request, safety, err := codeParseMessages(body, r, model, safetyMapping)
	if err != nil {
		return codePayload{}, err
	}
	return codeTranslateResponses(request, minimum, safety)
}

func codeTranslateChat(body []byte, r *http.Request, model string, minimum bool) (codePayload, error) {
	request, err := codeParseChat(body, r, model)
	if err != nil {
		return codePayload{}, err
	}
	return codeTranslateResponses(request, minimum, "")
}

func codeParseChat(body []byte, r *http.Request, model string) (*wire.Request, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || len(r.Header.Values("X-Codex-Turn-State")) != 0 || len(r.Header.Values("OpenAI-Beta")) != 0 {
		return nil, refused
	}
	request, err := protocols.ParseForModel(wire.Chat, body, model)
	if err != nil {
		return nil, refused
	}
	root := request.OIF().Document().Root()
	options, _ := root.Lookup("stream_options")
	if options.Kind() != oif.Absent && options.Kind() != oif.Null {
		if options.Kind() != oif.Object {
			return nil, refused
		}
		for _, member := range options.Members() {
			if member.Name != "include_usage" || member.Value.Kind() != oif.Boolean {
				return nil, refused
			}
		}
	}
	effort, _ := root.Lookup("reasoning_effort")
	if effort.Kind() != oif.Absent && effort.Kind() != oif.Null && !slices.Contains([]string{"low", "medium", "high"}, stringValue(effort)) {
		return nil, refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		role, _ := message.Lookup("role")
		name, _ := message.Lookup("name")
		content, _ := message.Lookup("content")
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "tool"}, stringValue(role)) || name.Kind() != oif.Absent && name.Kind() != oif.Null || content.Kind() != oif.Null && !codeTextContent(message, "content") {
			return nil, refused
		}
	}
	return request, nil
}

func stringValue(value oif.Value) string {
	text, _ := value.Text()
	return text
}

func codeEncodeResponses(request *wire.Request) (*wire.Request, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	encoded, _, encodeErr := protocols.EncodeTarget(request, wire.Responses, "openai", "openai", request.Route, nil)
	if encodeErr != nil {
		return nil, refused
	}
	target, targetErr := protocols.ParseForModel(wire.Responses, encoded, request.Route)
	if targetErr != nil {
		return nil, refused
	}

	return target, nil
}

func codeTranslateResponses(request *wire.Request, minimum bool, safety string) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	target, targetErr := codeEncodeResponses(request)
	if targetErr != nil {
		return codePayload{}, targetErr
	}

	input, _ := target.OIF().Document().Root().Lookup("input")
	roles := slices.Collect(func(yield func(oif.Change) bool) {
		for index, item := range input.Elements() {
			role, _ := item.Lookup("role")
			if text, _ := role.Text(); text == "system" && !yield(oif.Change{Pointer: fmt.Sprintf("/input/%d/role", index), Value: `"developer"`, Origin: oif.TransformedMapping, Reason: "Codex system instruction representation"}) {
				return
			}
		}
	})
	changes := slices.Concat([]oif.Change{
		{Pointer: "/stream", Value: "true", Origin: oif.TransportOption, Reason: "Codex subscription streaming transport"},
		{Pointer: "/store", Value: "false", Origin: oif.TransportOption, Reason: "Stateless code-mode translation"},
		{Pointer: "/instructions", Value: `""`, Origin: oif.TransportOption, Reason: "Codex required empty transport instructions"},
	}, roles, codeOutputMinimumChanges(target.OIF().Document().Root(), minimum), codeSafetyIdentifierChanges(safety))
	prepared, preparedErr := target.OIF().WithChanges(changes...)
	if preparedErr != nil {
		return codePayload{}, refused
	}
	return codePayload{body: prepared.Document().Bytes(), stream: request.Stream, translated: true, includeUsage: request.IncludeUsage}, nil
}

func codeMessagesContent(message oif.Value) bool {
	content, _ := message.Lookup("content")
	if content.Kind() == oif.String {
		return true
	}
	if content.Kind() != oif.Array {
		return false
	}
	for _, block := range content.Elements() {
		kind, _ := block.Lookup("type")
		switch name, _ := kind.Text(); name {
		case "text", "tool_use":
		case "tool_result":
			if !codeTextContent(block, "content") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func codeTextContent(parent oif.Value, key string) bool {
	value, present := parent.Lookup(key)
	if !present || value.Kind() == oif.String {
		return true
	}
	if value.Kind() != oif.Array {
		return false
	}
	for _, block := range value.Elements() {
		kind, _ := block.Lookup("type")
		name, _ := kind.Text()
		if name != "text" {
			return false
		}
	}
	return true
}

func (a *codeAttempt) translateResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, response *http.Response, model string, stream bool, family wire.Format, includeUsage bool) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		refusal := codeError(r, codemode.Refuse(response.StatusCode, "code_upstream_rejected"))
		surface := requestSurface(r)
		w.Header().Set("Content-Length", strconv.Itoa(len(refusal.surfaceBody(surface))))
		writeSurfaceError(w, refusal, surface)
		_ = http.NewResponseController(w).Flush()
		if response.StatusCode == http.StatusTooManyRequests {
			a.observeTranslatedRateLimit(ctx, response)
		}
		a.terminal = true
		return
	}
	writer := &streamWriter{w: w, family: family}
	completion, err := a.translatedCompletion(response, model, stream, family, includeUsage, writer.emit)
	if err != nil {
		if !a.terminal {
			a.outcome(ctx, codemode.Outcome{Origin: "upstream", Kind: "incomplete"})
		}
		writer.finish(&outcome{err: &Error{Status: 502, Code: "code_upstream_invalid", Type: "api_error", Message: "code_upstream_invalid"}, cancelled: ctx.Err() != nil})
		return
	}
	if stream {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(completion.Body)
}

func (a *codeAttempt) observeTranslatedRateLimit(ctx context.Context, response *http.Response) {
	body, err := readTranslatedRejection(ctx, response.Body, codewire.MaxBody+1)
	if err == nil {
		a.observeEncodedUnary(body, response.Header.Get("Content-Encoding"))
	}
}

func (a *codeAttempt) translatedCompletion(response *http.Response, model string, stream bool, family wire.Format, includeUsage bool, emit func([]byte) error) (*wire.Completion, error) {
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		observe := func(event oif.Event) error {
			a.observe(a.observer.Event(event.Source().Bytes()))
			return nil
		}
		if stream {
			return protocols.StreamWithEvents(wire.Responses, family, response.Body, codewire.MaxBody, model, includeUsage, emit, observe)
		}
		return protocols.AggregateTranslatedResponses(family, response.Body, codewire.MaxBody, model, observe)
	}
	if stream {
		return nil, codemode.Refuse(502, "code_upstream_invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, codewire.MaxBody+1))
	if err != nil || len(body) > codewire.MaxBody {
		return nil, codemode.Refuse(502, "code_upstream_invalid")
	}
	a.observeUnary(body)
	return protocols.Decode(wire.Responses, family, body, model, "")
}

func codeEffectiveOutputCap(cap int64, minimum bool) int64 {
	if minimum && cap > 0 {
		return max(16, cap)
	}
	return cap
}

func codeOutputMinimumChanges(root oif.Value, enabled bool) []oif.Change {
	value, _ := root.Lookup("max_output_tokens")
	cap, err := strconv.ParseInt(value.Raw(), 10, 64)
	if err != nil {
		return nil
	}
	effective := codeEffectiveOutputCap(cap, enabled)
	if effective == cap {
		return nil
	}
	return []oif.Change{{Pointer: "/max_output_tokens", Value: strconv.FormatInt(effective, 10), Origin: oif.TransformedMapping, Reason: "Published Codex translated output minimum"}}
}

func codeTargetEstimate(observation codemode.Request, body []byte, route codemode.Route, target codeTarget, ingress codeIngress) int64 {
	if !route.TranslatedCodexOutputMinimum || target.adapter.Adapter != codemode.AdapterCodex || target.endpoint.Protocol == ingress.protocol {
		return observation.Estimate
	}
	root, err := oif.ParseJSON(body, oif.Limits{MaxBytes: codewire.MaxBody})
	if err != nil {
		return observation.Estimate
	}
	cap := codewire.RateOutputCap(root.Root(), ingress.protocol)
	if cap > 0 {
		return observation.Estimate + codeEffectiveOutputCap(cap, true) - cap
	}
	return observation.Estimate
}

func codeMessagesSafetyIdentifier(request *wire.Request, enabled bool) (*wire.Request, string, error) {
	metadata, present := request.OIF().Document().Root().Lookup("metadata")
	if !present {
		return request, "", nil
	}
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if !enabled || metadata.Kind() != oif.Object || len(metadata.Members()) != 1 {
		return nil, "", refused
	}
	user, present := metadata.Lookup("user_id")
	identity, text := user.Text()
	if !present || !text || identity == "" {
		return nil, "", refused
	}
	prepared, err := request.WithFields(request.OIF().Document().FieldsWithout("metadata"), oif.TransformedMapping)
	if err != nil {
		return nil, "", refused
	}
	return prepared, fmt.Sprintf("%x", sha256.Sum256([]byte(identity))), nil
}

func codeSafetyIdentifierChanges(identifier string) []oif.Change {
	if identifier == "" {
		return nil
	}
	return []oif.Change{{Pointer: "/safety_identifier", Value: strconv.Quote(identifier), Origin: oif.TransformedMapping, Reason: "Explicit Messages abuse-detection identity mapping"}}
}
