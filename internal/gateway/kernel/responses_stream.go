package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

// A retrieved stream is a read of one authorized response, never another
// inference submission. Only the documented cursor and delivery selector may
// reach the historical provider; all other query controls fail before dispatch.
func responseRetrievalQuery(raw string) (url.Values, bool, *fault.Error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, false, fault.InvalidRequest("invalid_request", "The response retrieval query is malformed.", nil)
	}
	stream := false
	includes := []string{
		"file_search_call.results", "web_search_call.results", "web_search_call.action.sources",
		"message.input_image.image_url", "computer_call_output.output.image_url",
		"code_interpreter_call.outputs", "reasoning.encrypted_content", "message.output_text.logprobs",
	}
	for name, values := range query {
		if len(values) == 0 || len(values) > 16 || name != "include" && name != "include[]" && len(values) != 1 {
			return nil, false, fault.InvalidRequest("invalid_request", "Response retrieval query controls must occur once.", nil)
		}
		switch name {
		case "stream":
			if values[0] != "true" && values[0] != "false" {
				return nil, false, fault.InvalidRequest("invalid_request", "stream must be true or false.", nil)
			}
			stream = values[0] == "true"
		case "starting_after":
			if _, err := strconv.ParseUint(values[0], 10, 63); err != nil {
				return nil, false, fault.InvalidRequest("invalid_request", "starting_after must be a non-negative event sequence.", nil)
			}
		case "include_obfuscation":
			if values[0] != "true" && values[0] != "false" {
				return nil, false, fault.InvalidRequest("invalid_request", "include_obfuscation must be true or false.", nil)
			}
		case "include", "include[]":
			for _, value := range values {
				if !slices.Contains(includes, value) {
					return nil, false, fault.InvalidRequest("invalid_request", "The response include selector is not qualified.", nil)
				}
			}
		default:
			return nil, false, fault.InvalidRequest("invalid_request", "The response retrieval query has an unsupported control.", nil)
		}
	}
	if query.Has("include") && query.Has("include[]") {
		return nil, false, fault.InvalidRequest("invalid_request", "Use one include selector spelling for response retrieval.", nil)
	}
	if !stream && query.Has("starting_after") {
		return nil, false, fault.InvalidRequest("invalid_request", "starting_after requires stream=true.", nil)
	}
	if !stream {
		return query, false, nil
	}
	query.Set("stream", "true")
	return query, true, nil
}

// The native stream codec has already validated the SSE event grammar and
// bound its model. A non-strict resource read overlays only the response ID
// and an echoed previous_response_id, which previous resolves to the caller's
// local id ("" withholds it); untouched extensions and numeric lexemes stay
// byte-identical.
func projectStoredResponseFrame(frame []byte, projection responseProjection, strict bool, previous func(string) string) ([]byte, *openai.ResponseUsageObservation, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return nil, nil, errors.New("response stream frame has no data")
	}
	payload := bytes.TrimSpace(frame[i+7:])
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: len(frame) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, nil, errors.New("response stream frame is malformed")
	}
	nativeEvent, err := oif.NewEvent(wire.Descriptor(wire.Responses, true), doc, "", 0)
	if err != nil {
		return nil, nil, err
	}
	observation, lifecycle, err := openai.ObserveResponseEventUsage(nativeEvent)
	if err != nil {
		return nil, nil, err
	}
	if !lifecycle {
		return frame, nil, nil
	}
	response, present := doc.Root().Lookup("response")
	if !present {
		if kind, found := doc.Root().Lookup("type"); found {
			if name, valid := kind.Text(); valid && (name == "response.created" || name == "response.in_progress" || name == "response.queued") {
				return nil, nil, errors.New("response lifecycle event has no resource identity")
			}
		}
		return frame, nil, nil
	}
	if response.Kind() != oif.Object {
		return nil, nil, errors.New("response stream frame has no response object")
	}
	id, present := response.Lookup("id")
	identity, valid := id.Text()
	if !present || !valid || identity != projection.upstreamID {
		return nil, nil, errors.New("response stream changed resource identity")
	}
	var mapped oif.Document
	if strict {
		mapped, err = projection.project(doc, "/response")
	} else {
		local, _ := json.Marshal(projection.localID)
		changes := []oif.Change{{Pointer: "/response/id", Value: string(local), Origin: oif.ResourceBinding, Reason: "owner-scoped retained response"}}
		if prior, present := response.Lookup("previous_response_id"); present {
			if upstream, valid := prior.Text(); valid && upstream != "" {
				value := "null"
				if previous != nil {
					if mappedID := previous(upstream); mappedID != "" {
						value = quotedResponseID(mappedID)
					}
				}
				changes = append(changes, oif.Change{Pointer: "/response/previous_response_id", Value: value, Origin: oif.ResourceBinding, Reason: "owner-scoped prior response"})
			}
		}
		mapped, err = oif.Apply(doc, changes)
	}
	if err != nil {
		return nil, &observation, err
	}
	out := make([]byte, 0, i+7+mapped.Len()+2)
	out = append(out, frame[:i+7]...)
	out = append(out, mapped.Bytes()...)
	out = append(out, '\n', '\n')
	return out, &observation, nil
}

func redactFailedResponseFrame(frame []byte, credentials egress.Sensitive) ([]byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 || credentials.Contains(string(frame[:i])) {
		return nil, errors.New("failed response event has unsafe framing")
	}
	payload := bytes.TrimSpace(frame[i+7:])
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: len(frame) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, errors.New("failed response event is malformed")
	}
	redacted, err := fault.RedactNativeFailureDocument(doc, credentials)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, i+7+redacted.Len()+2)
	out = append(out, frame[:i+7]...)
	out = append(out, redacted.Bytes()...)
	out = append(out, '\n', '\n')
	return out, nil
}
