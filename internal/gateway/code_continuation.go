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
		values := make([]json.RawMessage, len(value.Elements()))
		for index, element := range value.Elements() {
			values[index] = canonicalContinuationJSON(element)
		}
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

func (a *codeAttempt) signedAntigravityResult(ctx context.Context, body []byte, model string) ([]byte, error) {
	refused := codemode.Refuse(502, "code_upstream_unsupported_state")
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, refused
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(envelope["response"], &result); err != nil {
		return nil, refused
	}
	var candidates []map[string]json.RawMessage
	if raw, present := result["candidates"]; present {
		if err := json.Unmarshal(raw, &candidates); err != nil {
			return nil, refused
		}
	}
	if len(candidates) > 1 {
		return envelope["response"], refused
	}
	groupID, groupErr := uuid.NewV7()
	if groupErr != nil {
		return envelope["response"], codemode.Refuse(502, "code_continuation_unavailable")
	}
	group := groupID.String()
	var calls []codesessions.SignedCall
	var changes []oif.Change
	for candidateIndex, candidate := range candidates {
		if _, exists := candidate["groundingMetadata"]; exists {
			return envelope["response"], refused
		}
		var content map[string]json.RawMessage
		if len(candidate["content"]) == 0 {
			continue
		}
		if err := json.Unmarshal(candidate["content"], &content); err != nil {
			return envelope["response"], refused
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(content["parts"], &parts); err != nil {
			return envelope["response"], refused
		}
		for partIndex, part := range parts {
			if len(part["functionCall"]) == 0 {
				if len(part) != 1 || len(part["text"]) == 0 {
					return envelope["response"], refused
				}
				continue
			}
			if len(part) > 2 {
				return envelope["response"], refused
			}
			for field := range part {
				if field != "functionCall" && field != "thoughtSignature" {
					return envelope["response"], refused
				}
			}
			var call continuationCall
			var signature string
			if err := json.Unmarshal(part["functionCall"], &call); err != nil {
				return envelope["response"], refused
			}
			if raw, present := part["thoughtSignature"]; present {
				if err := json.Unmarshal(raw, &signature); err != nil || signature == "" {
					return envelope["response"], refused
				}
			}
			callDocument, err := oif.ParseJSON(part["functionCall"], oif.Limits{})
			if err != nil || slices.ContainsFunc(callDocument.Root().Members(), func(member oif.Member) bool { return !slices.Contains([]string{"id", "name", "args"}, member.Name) }) {
				return envelope["response"], refused
			}
			digest, err := continuationDigest(call)
			if err != nil {
				return envelope["response"], refused
			}
			callID, idErr := uuid.NewV7()
			if idErr != nil {
				return envelope["response"], codemode.Refuse(502, "code_continuation_unavailable")
			}
			id := callID.String()
			calls = append(calls, codesessions.SignedCall{ID: id, Group: group, Index: len(calls), Digest: digest, ProviderID: call.ID, Signature: signature})
			pointer := "/candidates/" + strconv.Itoa(candidateIndex) + "/content/parts/" + strconv.Itoa(partIndex)
			outward, _ := json.Marshal(continuationCall{Name: call.Name, Args: call.Args, ID: &id})
			changes = append(changes, oif.Change{Pointer: pointer + "/functionCall", Value: string(outward), Origin: oif.QualifiedMapping, Reason: "bind tool continuation"})
			if _, present := part["thoughtSignature"]; present {
				changes = append(changes, oif.Change{Pointer: pointer + "/thoughtSignature", Remove: true, Origin: oif.QualifiedMapping, Reason: "retain provider state privately"})
			}
		}
	}
	document, parseErr := oif.ParseJSON(envelope["response"], oif.Limits{})
	if parseErr != nil {
		return envelope["response"], refused
	}
	transformed, err := oif.Apply(document, changes)
	if err != nil {
		return envelope["response"], refused
	}
	encoded := transformed.Bytes()
	if len(calls) == 0 {
		return encoded, nil
	}
	ledger, supported := a.server.deps.CodeLedger.(codeContinuationLedger)
	if !supported {
		return encoded, codemode.Refuse(502, "code_continuation_unavailable")
	}
	stored := make([]codesessions.SignedCall, len(calls))
	for index, call := range calls {
		stored[index] = codesessions.SignedCall{ID: call.ID, Group: call.Group, Index: call.Index, Count: len(calls), Digest: call.Digest, ProviderID: call.ProviderID, Signature: call.Signature}
	}
	if err := ledger.SaveCodeContinuation(ctx, a.permit, model, stored); err != nil {
		return encoded, codemode.Refuse(502, "code_continuation_unavailable")
	}
	return encoded, nil
}

type continuationContent struct {
	Role  string                       `json:"role"`
	Parts []map[string]json.RawMessage `json:"parts"`
}

func (a *codeAttempt) translateAntigravity(body []byte, r *http.Request, model, project string, signed bool) (codePayload, error) {
	payload, err := codeTranslateAntigravity(body, r, model, project)
	if err != nil || !signed {
		return payload, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload.body, &envelope); err != nil {
		return codePayload{}, err
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(envelope["request"], &request); err != nil {
		return codePayload{}, err
	}
	var contents []continuationContent
	if err := json.Unmarshal(request["contents"], &contents); err != nil {
		return codePayload{}, err
	}
	refused := codemode.Refuse(422, "code_continuation_unavailable")
	ledger, supported := a.server.deps.CodeLedger.(codeContinuationLedger)
	var represented []codesessions.SignedCall
	var returned []string
	var changes []oif.Change
	for contentIndex, content := range contents {
		var calls []codesessions.SignedCall
		for partIndex, part := range content.Parts {
			pointer := "/request/contents/" + strconv.Itoa(contentIndex) + "/parts/" + strconv.Itoa(partIndex)
			if raw, present := part["functionCall"]; present {
				if !supported || content.Role != "model" {
					return codePayload{}, refused
				}
				var call continuationCall
				if err := json.Unmarshal(raw, &call); err != nil || call.ID == nil {
					return codePayload{}, refused
				}
				if _, err := uuid.Parse(*call.ID); err != nil {
					return codePayload{}, refused
				}
				state, err := ledger.LoadCodeContinuation(r.Context(), a.permit, model, *call.ID)
				if err != nil {
					return codePayload{}, refused
				}
				digest, err := continuationDigest(call)
				if err != nil || state.Digest != digest || slices.ContainsFunc(represented, func(prior codesessions.SignedCall) bool { return prior.ID == state.ID }) {
					return codePayload{}, refused
				}
				calls = append(calls, state)
				represented = append(represented, state)
				restored, _ := json.Marshal(continuationCall{Name: call.Name, Args: call.Args, ID: state.ProviderID})
				changes = append(changes, oif.Change{Pointer: pointer + "/functionCall", Value: string(restored), Origin: oif.QualifiedMapping, Reason: "restore tool continuation"})
				if state.Signature != "" {
					signature, _ := json.Marshal(state.Signature)
					changes = append(changes, oif.Change{Pointer: pointer + "/thoughtSignature", Value: string(signature), Origin: oif.QualifiedMapping, Reason: "restore provider state"})
				}
			}
			if raw, present := part["functionResponse"]; present {
				var result map[string]json.RawMessage
				if err := json.Unmarshal(raw, &result); err != nil {
					return codePayload{}, refused
				}
				var id string
				if err := json.Unmarshal(result["id"], &id); err != nil {
					return codePayload{}, refused
				}
				index := slices.IndexFunc(represented, func(call codesessions.SignedCall) bool { return call.ID == id })
				if index < 0 || slices.Contains(returned, id) {
					return codePayload{}, refused
				}
				returned = append(returned, id)
				state := represented[index]
				if state.ProviderID == nil {
					changes = append(changes, oif.Change{Pointer: pointer + "/functionResponse/id", Remove: true, Origin: oif.QualifiedMapping, Reason: "restore provider identity"})
				} else {
					original, _ := json.Marshal(state.ProviderID)
					changes = append(changes, oif.Change{Pointer: pointer + "/functionResponse/id", Value: string(original), Origin: oif.QualifiedMapping, Reason: "restore provider identity"})
				}
			}
		}
		if !continuationGroupsComplete(calls) {
			return codePayload{}, refused
		}
	}
	document, parseErr := oif.ParseJSON(payload.body, oif.Limits{})
	if parseErr != nil {
		return codePayload{}, refused
	}
	transformed, err := oif.Apply(document, changes)
	if err != nil {
		return codePayload{}, refused
	}
	encoded := transformed.Bytes()
	return codePayload{body: encoded, query: payload.query, stream: payload.stream, includeUsage: payload.includeUsage, target: payload.target, signedContinuation: true}, nil
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
