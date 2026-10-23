package guardrails

import (
	"context"
	"encoding/json"
	"net/http"
)

var namedVendorsCore = map[string]namedVendor{
	"generic_guardrail_api": {evaluate: evaluateGenericGuardrail, transforms: true},
	"openai_moderation": {
		validate: func(raw json.RawMessage) error {
			var params moderationParameters
			return decodeVendorParameters(raw, &params)
		},
		evaluate: evaluateOpenAIModeration,
	},
}

type moderationParameters struct {
	Model string `json:"model,omitempty"`
}

func evaluateOpenAIModeration(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, _ string, s Segment) (Verdict, error) {
	var params moderationParameters
	if err := decodeVendorParameters(c.Parameters, &params); err != nil {
		return Verdict{}, err
	}
	body := map[string]any{"input": s.Text}
	if params.Model != "" {
		body["model"] = params.Model
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Results []struct {
			Flagged *bool `json:"flagged"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Results) != 1 || response.Results[0].Flagged == nil {
		return Verdict{}, errEvaluation
	}
	return vendorBoolean(*response.Results[0].Flagged, "vendor.openai_moderation"), nil
}

func evaluateGenericGuardrail(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	inputType := "request"
	if phase == Output || phase == OutputStream {
		inputType = "response"
	}
	body := map[string]any{"texts": []string{s.Text}, "input_type": inputType, "additional_provider_specific_params": map[string]any{}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "x-api-key", "")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Action string    `json:"action"`
		Texts  []*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Action {
	case "BLOCKED":
		return vendorBoolean(true, "vendor.generic_guardrail_api"), nil
	case "NONE":
		if len(response.Texts) == 0 {
			return vendorBoolean(false, ""), nil
		}
	case "GUARDRAIL_INTERVENED":
	default:
		return Verdict{}, errEvaluation
	}
	if len(response.Texts) != 1 || response.Texts[0] == nil {
		return Verdict{}, errEvaluation
	}
	if *response.Texts[0] == s.Text {
		return vendorBoolean(false, ""), nil
	}
	return vendorReplacement(s, *response.Texts[0], "vendor.generic_guardrail_api")
}
