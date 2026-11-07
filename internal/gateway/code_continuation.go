package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/oif"
)

type codeContinuationLedger interface {
	SaveCodeContinuation(context.Context, codesessions.Permit, string, []codesessions.SignedCall) error
	LoadCodeContinuation(context.Context, codesessions.Permit, string, string) (codesessions.SignedCall, error)
}

type continuationCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   *string         `json:"id,omitempty"`
}

func continuationDigest(call continuationCall) (string, error) {
	document, err := oif.ParseJSON(call.Args, oif.Limits{})
	if err != nil || document.Root().Kind() != oif.Object || call.Name == "" {
		return "", codemode.Refuse(422, "code_continuation_unavailable")
	}
	data, err := json.Marshal(struct {
		Name string
		Args json.RawMessage
	}{call.Name, canonicalContinuationJSON(document.Root())})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalContinuationJSON(value oif.Value) json.RawMessage {
	switch value.Kind() {
	case oif.Object:
		fields := maps.Collect(func(yield func(string, json.RawMessage) bool) {
			for _, member := range value.Members() {
				if !yield(member.Name, canonicalContinuationJSON(member.Value)) {
					return
				}
			}
		})
		data, _ := json.Marshal(fields)
		return data
	case oif.Array:
		values := slices.Collect(func(yield func(json.RawMessage) bool) {
			for _, element := range value.Elements() {
				if !yield(canonicalContinuationJSON(element)) {
					return
				}
			}
		})
		data, _ := json.Marshal(values)
		return data
	case oif.String:
		var text string
		_ = json.Unmarshal(value.Bytes(), &text)
		data, _ := json.Marshal(text)
		return data
	case oif.Number:
		number, err := decimal.NewFromString(string(value.Bytes()))
		if err != nil {
			return value.Bytes()
		}
		if number.IsZero() {
			return json.RawMessage("0")
		}
		coefficient := number.Coefficient().String()
		trimmed := strings.TrimRight(coefficient, "0")
		exponent := int64(number.Exponent()) + int64(len(coefficient)-len(trimmed))
		return json.RawMessage(trimmed + "e" + strconv.FormatInt(exponent, 10))
	default:
		return value.Bytes()
	}
}

type signedResponseCandidate struct {
	Grounding json.RawMessage `json:"groundingMetadata"`
	Content   *struct {
		Parts []json.RawMessage `json:"parts"`
	} `json:"content"`
}

type preparedSignedPart struct {
	call    *codesessions.SignedCall
	changes []oif.Change
	err     error
}

func prepareSignedResponsePart(raw json.RawMessage, pointer, group string) preparedSignedPart {
	refused := preparedSignedPart{err: codemode.Refuse(502, "code_upstream_unsupported_state")}
	document, err := oif.ParseJSON(raw, oif.Limits{})
	if err != nil || document.Root().Kind() != oif.Object {
		return refused
	}
	part := document.Root()
	function, isCall := part.Lookup("functionCall")
	if !isCall {
		text, present := part.Lookup("text")
		if !present || text.Kind() != oif.String || len(part.Members()) != 1 {
			return refused
		}
		return preparedSignedPart{}
	}
	if slices.ContainsFunc(part.Members(), func(field oif.Member) bool { return field.Name != "functionCall" && field.Name != "thoughtSignature" }) {
		return refused
	}
	if function.Kind() != oif.Object || slices.ContainsFunc(function.Members(), func(field oif.Member) bool { return !slices.Contains([]string{"id", "name", "args"}, field.Name) }) {
		return refused
	}
	if id, present := function.Lookup("id"); present && id.Kind() != oif.String {
		return refused
	}
	var call continuationCall
	if err := json.Unmarshal(function.Bytes(), &call); err != nil {
		return refused
	}
	signature, signatureErr := func() (string, error) {
		value, present := part.Lookup("thoughtSignature")
		if !present {
			return "", nil
		}
		var signature string
		if err := json.Unmarshal(value.Bytes(), &signature); err != nil || signature == "" {
			return "", refused.err
		}
		return signature, nil
	}()
	if signatureErr != nil {
		return refused
	}
	digest, err := continuationDigest(call)
	if err != nil {
		return refused
	}
	callID, err := uuid.NewV7()
	if err != nil {
		return preparedSignedPart{err: codemode.Refuse(502, "code_continuation_unavailable")}
	}
	id := callID.String()
	outward, _ := json.Marshal(continuationCall{Name: call.Name, Args: call.Args, ID: &id})
	callChange := oif.Change{Pointer: pointer + "/functionCall", Value: string(outward), Origin: oif.QualifiedMapping, Reason: "bind tool continuation"}
	changes := func() []oif.Change {
		if signature == "" {
			return []oif.Change{callChange}
		}
		return []oif.Change{callChange, {Pointer: pointer + "/thoughtSignature", Remove: true, Origin: oif.QualifiedMapping, Reason: "retain provider state privately"}}
	}()
	return preparedSignedPart{call: &codesessions.SignedCall{ID: id, Group: group, Digest: digest, ProviderID: call.ID, Signature: signature}, changes: changes}
}

func prepareSignedResponse(candidates []signedResponseCandidate, group string) []preparedSignedPart {
	return slices.Collect(func(yield func(preparedSignedPart) bool) {
		for candidateIndex, candidate := range candidates {
			if len(candidate.Grounding) != 0 {
				yield(preparedSignedPart{err: codemode.Refuse(502, "code_upstream_unsupported_state")})
				return
			}
			if candidate.Content == nil {
				continue
			}
			for partIndex, part := range candidate.Content.Parts {
				pointer := "/candidates/" + strconv.Itoa(candidateIndex) + "/content/parts/" + strconv.Itoa(partIndex)
				if !yield(prepareSignedResponsePart(part, pointer, group)) {
					return
				}
			}
		}
	})
}

func (a *codeAttempt) signedAntigravityResult(ctx context.Context, body []byte, model string) ([]byte, error) {
	refused := codemode.Refuse(502, "code_upstream_unsupported_state")
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, refused
	}
	var result struct {
		Candidates []signedResponseCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(envelope.Response, &result); err != nil || len(result.Candidates) > 1 {
		return envelope.Response, refused
	}
	group, err := uuid.NewV7()
	if err != nil {
		return envelope.Response, codemode.Refuse(502, "code_continuation_unavailable")
	}
	prepared := prepareSignedResponse(result.Candidates, group.String())
	if index := slices.IndexFunc(prepared, func(part preparedSignedPart) bool { return part.err != nil }); index >= 0 {
		return envelope.Response, prepared[index].err
	}
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		for _, part := range prepared {
			for _, change := range part.changes {
				if !yield(change) {
					return
				}
			}
		}
	})
	document, err := oif.ParseJSON(envelope.Response, oif.Limits{})
	if err != nil {
		return envelope.Response, refused
	}
	transformed, err := oif.Apply(document, changes)
	if err != nil {
		return envelope.Response, refused
	}
	calls := slices.Collect(func(yield func(codesessions.SignedCall) bool) {
		for _, part := range prepared {
			if part.call != nil && !yield(*part.call) {
				return
			}
		}
	})
	if len(calls) == 0 {
		return transformed.Bytes(), nil
	}
	ledger, supported := a.server.deps.CodeLedger.(codeContinuationLedger)
	if !supported {
		return transformed.Bytes(), codemode.Refuse(502, "code_continuation_unavailable")
	}
	stored := slices.Collect(func(yield func(codesessions.SignedCall) bool) {
		for index, call := range calls {
			if !yield(codesessions.SignedCall{ID: call.ID, Group: call.Group, Index: index, Count: len(calls), Digest: call.Digest, ProviderID: call.ProviderID, Signature: call.Signature}) {
				return
			}
		}
	})
	if err := ledger.SaveCodeContinuation(ctx, a.permit, model, stored); err != nil {
		return transformed.Bytes(), codemode.Refuse(502, "code_continuation_unavailable")
	}
	return transformed.Bytes(), nil
}

type continuationResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type continuationPart struct {
	Call     *continuationCall     `json:"functionCall"`
	Response *continuationResponse `json:"functionResponse"`
}

type continuationContent struct {
	Role  string             `json:"role"`
	Parts []continuationPart `json:"parts"`
}

type locatedContinuationPart struct {
	continuationPart
	role    string
	content int
	part    int
}

func (p locatedContinuationPart) pointer() string {
	return "/request/contents/" + strconv.Itoa(p.content) + "/parts/" + strconv.Itoa(p.part)
}

func (p locatedContinuationPart) before(other locatedContinuationPart) bool {
	return p.content < other.content || p.content == other.content && p.part < other.part
}

type restoredContinuation struct {
	entry   locatedContinuationPart
	state   codesessions.SignedCall
	changes []oif.Change
	err     error
}

func continuationEntries(contents []continuationContent) []locatedContinuationPart {
	return slices.Collect(func(yield func(locatedContinuationPart) bool) {
		for contentIndex, content := range contents {
			for partIndex, part := range content.Parts {
				if !yield(locatedContinuationPart{continuationPart: part, role: content.Role, content: contentIndex, part: partIndex}) {
					return
				}
			}
		}
	})
}

func (a *codeAttempt) restoreContinuationCall(ctx context.Context, entry locatedContinuationPart, model string) restoredContinuation {
	refused := restoredContinuation{err: codemode.Refuse(422, "code_continuation_unavailable")}
	ledger, supported := a.server.deps.CodeLedger.(codeContinuationLedger)
	call := entry.Call
	if !supported || entry.role != "model" || call.ID == nil || entry.Response != nil {
		return refused
	}
	if _, err := uuid.Parse(*call.ID); err != nil {
		return refused
	}
	state, err := ledger.LoadCodeContinuation(ctx, a.permit, model, *call.ID)
	if err != nil {
		return refused
	}
	digest, err := continuationDigest(*call)
	if err != nil || state.ID != *call.ID || state.Digest != digest {
		return refused
	}
	restored, _ := json.Marshal(continuationCall{Name: call.Name, Args: call.Args, ID: state.ProviderID})
	signature, _ := json.Marshal(state.Signature)
	changes := func() []oif.Change {
		callChange := oif.Change{Pointer: entry.pointer() + "/functionCall", Value: string(restored), Origin: oif.QualifiedMapping, Reason: "restore tool continuation"}
		if state.Signature == "" {
			return []oif.Change{callChange}
		}
		return []oif.Change{callChange, {Pointer: entry.pointer() + "/thoughtSignature", Value: string(signature), Origin: oif.QualifiedMapping, Reason: "restore provider state"}}
	}()
	return restoredContinuation{entry: entry, state: state, changes: changes}
}

func restoreContinuationResult(entry locatedContinuationPart, calls []restoredContinuation) restoredContinuation {
	refused := restoredContinuation{err: codemode.Refuse(422, "code_continuation_unavailable")}
	index := slices.IndexFunc(calls, func(call restoredContinuation) bool { return call.state.ID == entry.Response.ID })
	if index < 0 || entry.role != "user" {
		return refused
	}
	call := calls[index]
	if !call.entry.before(entry) || entry.Response.Name != call.entry.Call.Name {
		return refused
	}
	original, _ := json.Marshal(call.state.ProviderID)
	change := oif.Change{Pointer: entry.pointer() + "/functionResponse/id", Value: string(original), Origin: oif.QualifiedMapping, Reason: "restore provider identity"}
	if call.state.ProviderID == nil {
		return restoredContinuation{entry: entry, changes: []oif.Change{{Pointer: change.Pointer, Remove: true, Origin: change.Origin, Reason: change.Reason}}}
	}
	return restoredContinuation{entry: entry, changes: []oif.Change{change}}
}

func validateContinuationHistory(contents []continuationContent, calls, results []restoredContinuation) error {
	refused := codemode.Refuse(422, "code_continuation_unavailable")
	if len(calls) != len(results) {
		return refused
	}
	for index, call := range calls {
		if call.err != nil {
			return call.err
		}
		if slices.IndexFunc(calls, func(prior restoredContinuation) bool { return prior.state.ID == call.state.ID }) != index {
			return refused
		}
	}
	for index, result := range results {
		if result.err != nil {
			return result.err
		}
		if slices.IndexFunc(results, func(prior restoredContinuation) bool { return prior.entry.Response.ID == result.entry.Response.ID }) != index {
			return refused
		}
	}
	for contentIndex := range contents {
		group := slices.Collect(func(yield func(codesessions.SignedCall) bool) {
			for _, call := range calls {
				if call.entry.content == contentIndex && !yield(call.state) {
					return
				}
			}
		})
		if !continuationGroupsComplete(group) {
			return refused
		}
	}
	return nil
}

func (a *codeAttempt) translateAntigravity(body []byte, r *http.Request, model, project string, signed bool) (codePayload, error) {
	payload, err := codeTranslateAntigravity(body, r, model, project)
	if err != nil || !signed {
		return payload, err
	}
	var envelope struct {
		Request struct {
			Contents []continuationContent `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(payload.body, &envelope); err != nil {
		return codePayload{}, err
	}
	entries := continuationEntries(envelope.Request.Contents)
	calls := slices.Collect(func(yield func(restoredContinuation) bool) {
		for _, entry := range entries {
			if entry.Call != nil && !yield(a.restoreContinuationCall(r.Context(), entry, model)) {
				return
			}
		}
	})
	if index := slices.IndexFunc(calls, func(call restoredContinuation) bool { return call.err != nil }); index >= 0 {
		return codePayload{}, calls[index].err
	}
	results := slices.Collect(func(yield func(restoredContinuation) bool) {
		for _, entry := range entries {
			if entry.Response != nil && !yield(restoreContinuationResult(entry, calls)) {
				return
			}
		}
	})
	if err := validateContinuationHistory(envelope.Request.Contents, calls, results); err != nil {
		return codePayload{}, err
	}
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		for _, restored := range slices.Concat(calls, results) {
			for _, change := range restored.changes {
				if !yield(change) {
					return
				}
			}
		}
	})
	refused := codemode.Refuse(422, "code_continuation_unavailable")
	document, err := oif.ParseJSON(payload.body, oif.Limits{})
	if err != nil {
		return codePayload{}, refused
	}
	transformed, err := oif.Apply(document, changes)
	if err != nil {
		return codePayload{}, refused
	}
	return codePayload{body: transformed.Bytes(), query: payload.query, stream: payload.stream, includeUsage: payload.includeUsage, target: payload.target, signedContinuation: true}, nil
}

func continuationGroupsComplete(calls []codesessions.SignedCall) bool {
	if len(calls) == 0 {
		return true
	}
	first := calls[0]
	if first.Index != 0 || first.Count < 1 || first.Count > len(calls) {
		return false
	}
	for index, call := range calls[:first.Count] {
		if call.Group != first.Group || call.Index != index || call.Count != first.Count {
			return false
		}
	}
	if slices.ContainsFunc(calls[first.Count:], func(call codesessions.SignedCall) bool { return call.Group == first.Group }) {
		return false
	}
	return continuationGroupsComplete(calls[first.Count:])
}
