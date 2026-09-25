package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

// A retrieved stream is a read of one authorized response, never another
// inference submission. Only the documented cursor and delivery selector may
// reach the historical provider; all other query controls fail before dispatch.
func responseRetrievalQuery(raw string) (url.Values, bool, *Error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, false, invalidRequest("invalid_request", "The response retrieval query is malformed.", nil)
	}
	stream := false
	includes := []string{
		"file_search_call.results", "web_search_call.results", "web_search_call.action.sources",
		"message.input_image.image_url", "computer_call_output.output.image_url",
		"code_interpreter_call.outputs", "reasoning.encrypted_content", "message.output_text.logprobs",
	}
	for name, values := range query {
		if len(values) == 0 || len(values) > 16 || name != "include" && name != "include[]" && len(values) != 1 {
			return nil, false, invalidRequest("invalid_request", "Response retrieval query controls must occur once.", nil)
		}
		switch name {
		case "stream":
			if values[0] != "true" && values[0] != "false" {
				return nil, false, invalidRequest("invalid_request", "stream must be true or false.", nil)
			}
			stream = values[0] == "true"
		case "starting_after":
			if _, err := strconv.ParseUint(values[0], 10, 63); err != nil {
				return nil, false, invalidRequest("invalid_request", "starting_after must be a non-negative event sequence.", nil)
			}
		case "include_obfuscation":
			if values[0] != "true" && values[0] != "false" {
				return nil, false, invalidRequest("invalid_request", "include_obfuscation must be true or false.", nil)
			}
		case "include", "include[]":
			for _, value := range values {
				if !slices.Contains(includes, value) {
					return nil, false, invalidRequest("invalid_request", "The response include selector is not qualified.", nil)
				}
			}
		default:
			return nil, false, invalidRequest("invalid_request", "The response retrieval query has an unsupported control.", nil)
		}
	}
	if query.Has("include") && query.Has("include[]") {
		return nil, false, invalidRequest("invalid_request", "Use one include selector spelling for response retrieval.", nil)
	}
	if !stream && query.Has("starting_after") {
		return nil, false, invalidRequest("invalid_request", "starting_after requires stream=true.", nil)
	}
	if !stream {
		return query, false, nil
	}
	query.Set("stream", "true")
	return query, true, nil
}

// The native stream codec has already validated the SSE event grammar and
// bound its model. One response-ID overlay is the only additional change on a
// resource read; untouched extensions and numeric lexemes stay byte-identical.
func projectStoredResponseFrame(frame []byte, projection responseProjection, strict bool) ([]byte, []byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return nil, nil, fmt.Errorf("%w: response stream frame has no data", errResponseMapping)
	}
	payload := bytes.TrimSpace(frame[i+7:])
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: len(frame) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, nil, fmt.Errorf("%w: response stream frame is malformed", errResponseMapping)
	}
	response, present := doc.Root().Lookup("response")
	if !present {
		if kind, found := doc.Root().Lookup("type"); found {
			if name, valid := kind.Text(); valid && (name == "response.created" || name == "response.in_progress" || name == "response.queued") {
				return nil, nil, fmt.Errorf("%w: response lifecycle event has no resource identity", errResponseMapping)
			}
		}
		return frame, nil, nil
	}
	if response.Kind() != oif.Object {
		return nil, nil, fmt.Errorf("%w: response stream frame has no response object", errResponseMapping)
	}
	id, present := response.Lookup("id")
	identity, valid := id.Text()
	if !present || !valid || identity != projection.upstreamID {
		return nil, nil, fmt.Errorf("%w: response stream changed resource identity", errResponseMapping)
	}
	var mapped oif.Document
	if strict {
		mapped, err = projection.project(doc, "/response")
	} else {
		local, _ := json.Marshal(projection.localID)
		mapped, err = oif.Apply(doc, []oif.Change{{Pointer: "/response/id", Value: string(local), Origin: oif.ResourceBinding, Reason: "owner-scoped retained response"}})
	}
	if err != nil {
		return nil, nil, err
	}
	out := make([]byte, 0, i+7+mapped.Len()+2)
	out = append(out, frame[:i+7]...)
	out = append(out, mapped.Bytes()...)
	out = append(out, '\n', '\n')
	return out, response.Bytes(), nil
}

// A provider error can echo authentication with arbitrary JSON escape
// spelling. Redact decoded strings while OIF keeps every unrelated member and
// number byte-for-byte. A credential-bearing member name cannot be renamed
// without changing the native grammar, so refuse it.
func redactNativeFailureDocument(doc oif.Document, credentials []string) (oif.Document, error) {
	changes := []oif.Change{}
	var inspect func(oif.Value, string) error
	inspect = func(value oif.Value, pointer string) error {
		switch value.Kind() {
		case oif.Object:
			for _, member := range value.Members() {
				if redactCredentials(member.Name, credentials) != member.Name {
					return errors.New("failed response event has a credential-bearing member name")
				}
				if err := inspect(member.Value, oif.Pointer(pointer, member.Name)); err != nil {
					return err
				}
			}
		case oif.Array:
			for index, item := range value.Elements() {
				if err := inspect(item, oif.Pointer(pointer, strconv.Itoa(index))); err != nil {
					return err
				}
			}
		case oif.String:
			text, ok := value.Text()
			if !ok {
				return errors.New("failed response event has invalid text")
			}
			if safe := redactCredentials(text, credentials); safe != text {
				encoded, _ := json.Marshal(safe)
				changes = append(changes, oif.Change{Pointer: pointer, Value: string(encoded), Origin: oif.ExplicitTransform, Reason: "provider error credential redaction"})
			}
		}
		return nil
	}
	if err := inspect(doc.Root(), ""); err != nil {
		return oif.Document{}, err
	}
	if len(changes) == 0 {
		return doc, nil
	}
	return oif.Apply(doc, changes)
}

func redactFailedResponseFrame(frame []byte, credentials []string) ([]byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 || redactCredentials(string(frame[:i]), credentials) != string(frame[:i]) {
		return nil, errors.New("failed response event has unsafe framing")
	}
	payload := bytes.TrimSpace(frame[i+7:])
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: len(frame) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, errors.New("failed response event is malformed")
	}
	redacted, err := redactNativeFailureDocument(doc, credentials)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, i+7+redacted.Len()+2)
	out = append(out, frame[:i+7]...)
	out = append(out, redacted.Bytes()...)
	out = append(out, '\n', '\n')
	return out, nil
}

// Retained-stream faults that originate in this gateway rather than the
// provider: replay projection defects and durability of the local resource
// state. They never charge the provider's shared circuit.
var (
	errRetainedProjection = errors.New("retained response event could not be projected")
	errRetainedCommit     = errors.New("retained response terminal state could not be committed")
)

func (s *Server) responseCredentialValues(x *execution, p *pin, response *http.Response) []string {
	values := []string{string(s.pinSecret(x, p))}
	if response.Request != nil {
		names := append([]string{"Authorization", "Api-Key", "X-Goog-Api-Key"}, p.provider.CredentialHeaders...)
		for _, name := range names {
			values = append(values, response.Request.Header.Values(name)...)
		}
	}
	return values
}

func (s *Server) streamStoredResponse(ctx context.Context, w http.ResponseWriter, x *execution, res *resources.Resource, p *pin, query url.Values) *Error {
	if res.Kind == resources.KindStrictResponse &&
		(!p.provider.Supports(p.model, "generation", "openai", "streaming") || !p.provider.Connector().Supports("generation", "openai", "streaming")) {
		return invalidRequest("target_capability", "The retained provider has no qualified Responses stream contract.", nil)
	}
	projection := responseProjection{upstreamID: res.UpstreamID, localID: res.ID, route: res.RouteSlug}
	if res.Kind == resources.KindStrictResponse {
		var parentErr error
		projection.previousUpstream, projection.previousLocal, parentErr = responseParentProjection(res, x.responseContract)
		if parentErr != nil {
			return serverError(http.StatusConflict, "provider_resource_unavailable", "The retained parent response cannot be reconstructed.")
		}
	}
	endpoint, err := resourceURL(p.provider.Connector(), p.model, responsePath(p.provider.Connector(), "/"+url.PathEscape(res.UpstreamID)), query)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	x.mode = "streaming"
	resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.status == http.StatusNotFound {
			return notFoundError("not_found", "No stored response with this identifier exists for this key.")
		}
		return upstreamError(failure)
	}
	defer resp.Body.Close()
	x.dispatched = true
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		// The provider answered a stream endpoint without an event stream;
		// that is malformed provider data, not a projection defect.
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope = faultProviderTransport, scopeEndpoint
		}
		return serverError(http.StatusBadGateway, "provider_protocol_error", "The provider did not return a Responses event stream.")
	}
	limit := int(s.cfg.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	committed := false
	nativeTerminalFailure := false
	fact := &x.facts[len(x.facts)-1]
	credentialValues := s.responseCredentialValues(x, p, resp)
	emit := func(frame []byte) error {
		projected, original, err := projectStoredResponseFrame(frame, projection, res.Kind == resources.KindStrictResponse)
		if err != nil {
			if errors.Is(err, errResponseMapping) {
				// Malformed or drifting provider frame data is endpoint
				// evidence, not a projection defect of this gateway.
				return err
			}
			return fmt.Errorf("%w: %v", errRetainedProjection, err)
		}
		if len(projected) > limit {
			return openai.ErrEventTooLarge
		}
		if original != nil {
			if status, ok := upstreamString(original, "status"); ok {
				if status == "failed" {
					nativeTerminalFailure = true
				}
				switch status {
				case "completed", "failed", "incomplete", "cancelled":
					// A terminal status the provider declared is outcome
					// evidence; "incomplete" is a valid terminal, never a
					// fault of the replay.
					fact.NativeStatus = status
					commitCtx, stopCommit := resourceCommitContext(ctx)
					reconcileErr := s.reconcileResponse(commitCtx, res.ID, original)
					var updateErr error
					if reconcileErr == nil {
						updateErr = s.Resources.Update(commitCtx, res.ID, status, nil, nil)
					}
					stopCommit()
					if reconcileErr != nil || updateErr != nil {
						return errRetainedCommit
					}
				}
			}
		}
		if bytes.Contains(projected, []byte("event: response.failed\n")) || bytes.Contains(projected, []byte("event: error\n")) {
			projected, err = redactFailedResponseFrame(projected, credentialValues)
			if err != nil {
				return fmt.Errorf("%w: %v", errRetainedProjection, err)
			}
			if len(projected) > limit {
				return openai.ErrEventTooLarge
			}
		}
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			committed = true
		}
		if _, err := w.Write(projected); err != nil {
			return fmt.Errorf("%w: %v", errClientWrite, err)
		}
		x.delivered(s.now())
		if fact.Interaction != nil {
			fact.Interaction.ClientState = usage.ClientPartial
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return fmt.Errorf("%w: %v", errClientWrite, err)
		}
		return nil
	}
	_, streamErr := protocols.StreamWithEvents(openai.FamilyResponses, openai.FamilyResponses,
		p.provider.Connector().StreamPayload(resp.Body, limit), limit, x.route.Slug, true, emit, nil)
	if streamErr != nil {
		out := &outcome{committed: committed}
		var upstream *openai.UpstreamError
		switch {
		case errors.Is(streamErr, errClientWrite):
			// The caller went away mid-replay; nothing about the provider
			// changed and nothing more can be delivered.
			fact.Class = classCancelled
			fact.FaultOrigin, fact.FaultScope = faultClientDelivery, scopeRequest
			x.failure = (&attemptFailure{class: classCancelled}).toError()
			out.err, out.cancelled = x.failure, true
		case errors.Is(streamErr, errRetainedCommit):
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope = faultProxyPersistence, scopeRequest
			x.failure = serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The gateway could not durably record the provider outcome.")
			out.err = x.failure
		case errors.Is(streamErr, openai.ErrEventTooLarge):
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope, fact.FaultResource = faultProxyCapacity, scopeRequest, "event bytes"
			x.failure = serverError(http.StatusServiceUnavailable, "proxy_resource_exhausted", "The provider exchange exceeded the gateway's event bytes limit.")
			out.err = x.failure
		case errors.Is(streamErr, errRetainedProjection):
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope = faultContract, scopeContract
			x.failure = serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained response stream could not be projected faithfully.")
			out.err = x.failure
		case errors.Is(streamErr, errResponseMapping):
			// The retained provider's own frames were malformed or drifted
			// from the accepted identity; that is endpoint evidence.
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope = faultProviderTransport, scopeEndpoint
			x.failure = serverError(http.StatusBadGateway, "provider_protocol_error", "The retained provider stream returned malformed data.")
			out.err = x.failure
		case errors.As(streamErr, &upstream) && nativeTerminalFailure:
			x.failure = serverError(http.StatusBadGateway, "upstream_response_failed", "The retained provider reported a failed response.")
			fact.Class = classUpstreamServer
			fact.FaultOrigin, fact.FaultScope = faultNativeOutcome, scopeEndpoint
			out.err = x.failure
		case errors.As(streamErr, &upstream):
			// The provider declared an in-band error envelope without a
			// failed terminal state; that is still provider evidence.
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope = faultProviderDeclared, scopeEndpoint
			x.failure = serverError(http.StatusBadGateway, "response_stream_incomplete", "The retained response stream ended before its terminal contract.")
			out.err = x.failure
		default:
			x.failure = serverError(http.StatusBadGateway, "response_stream_incomplete", "The retained response stream ended before its terminal contract.")
			fact.Class = classProtocol
			fact.FaultOrigin, fact.FaultScope = faultProviderTransport, scopeEndpoint
			out.err = x.failure
		}
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = usage.UpstreamUnknown
			if nativeTerminalFailure {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
			}
		}
		if !committed {
			return x.failure
		}
		s.finish(x, out, http.StatusOK)
		return nil
	}
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState, fact.Interaction.ClientState = usage.UpstreamTerminal, usage.ClientTerminal
	}
	s.finish(x, &outcome{committed: true}, http.StatusOK)
	return nil
}
