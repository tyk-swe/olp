package playground

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

func routingEvidence(trail kernel.MemberTrail) []map[string]any {
	evidenceRows := []map[string]any{}
	for _, fact := range trail.Facts {
		var evidence routing.Decision
		for _, decision := range trail.Decisions {
			if decision.TargetID == fact.TargetID && decision.CredentialSlotID != nil && *decision.CredentialSlotID == fact.SlotID {
				evidence = decision
				break
			}
		}
		evidenceRows = append(evidenceRows, map[string]any{
			"target_id":      fact.TargetID,
			"provider_id":    fact.ProviderID,
			"upstream_model": fact.UpstreamModel,
			"eligible":       true,
			"priority":       attemptPriority(trail, fact.TargetID),
			"strategy":       fact.Strategy,
			"price":          fact.Price, "vendor_id": fact.VendorID, "performance": evidence.Performance, "metadata_observed_at": evidence.MetadataObservedAt,
			"attempt":            fact.Ordinal,
			"credential_slot_id": fact.SlotID,
			"reason":             fact.Class,
		})
	}
	return evidenceRows
}

func response(trail kernel.MemberTrail, c *wire.Completion, in Request) map[string]any {
	routing := routingEvidence(trail)
	if in.operation() != "generation" {
		res := map[string]any{
			"id":          trail.ID,
			"model":       trail.Route,
			"output_text": "",
			"tool_calls":  []map[string]any{},
			"latency_ms":  trail.Elapsed.Milliseconds(),
			"routing":     routing,
		}
		if json.Valid(c.Body) {
			// Keep the operation-owned native result intact. Decoding to any
			// rounds scores, vector values and large integer metadata to float64.
			res["response"] = json.RawMessage(c.Body)
			res["response_raw"] = string(c.Body)
		}
		if c.Usage != nil {
			res["usage"] = c.Usage
		}
		return res
	}
	tools := []map[string]any{}
	for _, call := range c.ToolCalls {
		tools = append(tools, map[string]any{"id": call.ID, "name": call.Name, "arguments": call.Arguments})
	}
	res := map[string]any{
		"id":          trail.ID,
		"model":       trail.Route,
		"output_text": c.OutputText,
		"tool_calls":  tools,
		"latency_ms":  trail.Elapsed.Milliseconds(),
		"routing":     routing,
	}
	if c.FinishReason != "" {
		res["finish_reason"] = c.FinishReason
	}
	if c.ProviderModel != "" {
		res["provider_model"] = c.ProviderModel
	}
	if c.Refusal != "" {
		res["refusal"] = c.Refusal
	}
	if c.Usage != nil {
		res["usage"] = c.Usage
	}
	if in.ResponseFormat != nil && in.ResponseFormat.Type != "text" && c.OutputText != "" {
		decoder := json.NewDecoder(strings.NewReader(c.OutputText))
		decoder.UseNumber()
		var structured any
		if decoder.Decode(&structured) == nil && decoder.Decode(new(any)) == io.EOF {
			res["structured_output"] = structured
		}
	}
	return res
}

func attemptPriority(trail kernel.MemberTrail, targetID string) int {
	for _, a := range trail.Attempts {
		if a.TargetID == targetID {
			return a.Priority
		}
	}
	return 0
}
