package vendor

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

func guardrailsAI(ctx context.Context, x exchange, r Request) (Decision, error) {
	raw, err := x.bearer(ctx, map[string]string{"llmOutput": r.Text})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Passed *bool   `json:"validationPassed"`
		Output *string `json:"validatedOutput"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Passed == nil {
		return Decision{}, errUnavailable
	}
	if !*response.Passed {
		return flag("guardrails_ai.validation"), nil
	}
	if response.Output == nil || *response.Output == r.Text {
		return Decision{}, nil
	}
	return replace(r, *response.Output, "guardrails_ai.validation")
}

type lakeraParameters struct {
	CategoryThresholds map[string]float64 `json:"category_thresholds,omitempty"`
}

func (p lakeraParameters) valid() bool {
	for category, threshold := range p.CategoryThresholds {
		if (category != "jailbreak" && category != "prompt_injection") || !unit(threshold) {
			return false
		}
	}
	return true
}

func lakeraV1(ctx context.Context, x exchange, p lakeraParameters, r Request) (Decision, error) {
	raw, err := x.bearer(ctx, map[string]string{"input": r.Text})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Results []struct {
			Flagged *bool               `json:"flagged"`
			Scores  map[string]*float64 `json:"category_scores"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Results) != 1 || response.Results[0].Flagged == nil {
		return Decision{}, errUnavailable
	}
	if len(p.CategoryThresholds) == 0 {
		return flagIf(*response.Results[0].Flagged, "lakera.prompt_injection"), nil
	}
	exceeded := false
	for category, threshold := range p.CategoryThresholds {
		score := response.Results[0].Scores[category]
		if score == nil || !unit(*score) {
			return Decision{}, errUnavailable
		}
		exceeded = exceeded || *score >= threshold
	}
	return flagIf(exceeded, "lakera.prompt_injection"), nil
}

type panwParameters struct {
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
}

func (p panwParameters) valid() bool { return label(p.ProfileID) && label(p.ProfileName) }

func panw(ctx context.Context, x exchange, p panwParameters, r Request) (Decision, error) {
	body := map[string]any{"contents": []map[string]string{{pick(r, "prompt", "response"): r.Text}}, "metadata": map[string]any{"is_response": r.Output}}
	profile := map[string]string{}
	if p.ProfileID != "" {
		profile["profile_id"] = p.ProfileID
	}
	if p.ProfileName != "" {
		profile["profile_name"] = p.ProfileName
	}
	if len(profile) > 0 {
		body["ai_profile"] = profile
	}
	raw, err := x.keyed(ctx, body, "x-pan-token", "")
	if err != nil {
		return Decision{}, err
	}
	type masked struct {
		Data *string `json:"data"`
	}
	var response struct {
		Action   string          `json:"action"`
		Prompt   *masked         `json:"prompt_masked_data"`
		Response *masked         `json:"response_masked_data"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil || failed(response.Error) {
		return Decision{}, errUnavailable
	}
	decision, err := allowBlock(response.Action, "panw_prisma_airs.policy")
	if err != nil {
		return Decision{}, err
	}
	replacement := response.Prompt
	if r.Output {
		replacement = response.Response
	}
	if replacement == nil {
		return decision, nil
	}
	if replacement.Data == nil {
		return Decision{}, errUnavailable
	}
	if *replacement.Data == r.Text {
		return decision, nil
	}
	return replace(r, *replacement.Data, "panw_prisma_airs.policy")
}

type zscalerParameters struct {
	PolicyID int64 `json:"policy_id,omitempty"`
}

func (p zscalerParameters) valid() bool { return p.PolicyID >= 0 }

func zscaler(ctx context.Context, x exchange, p zscalerParameters, r Request) (Decision, error) {
	body := map[string]any{"direction": pick(r, "IN", "OUT"), "content": r.Text}
	if p.PolicyID > 0 {
		body["policyId"] = p.PolicyID
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Status int    `json:"statusCode"`
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Status != http.StatusOK {
		return Decision{}, errUnavailable
	}
	switch response.Action {
	case "ALLOW":
		return Decision{}, nil
	case "DETECT", "BLOCK":
		return flag("zscaler_ai_guard.policy"), nil
	default:
		return Decision{}, errUnavailable
	}
}

type javelinParameters struct {
	Application string          `json:"application,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
}

func (p javelinParameters) valid() bool {
	if !label(p.Application) {
		return false
	}
	if len(p.Config) == 0 {
		return true
	}
	var config map[string]json.RawMessage
	return json.Unmarshal(p.Config, &config) == nil && config != nil
}

func javelin(ctx context.Context, x exchange, p javelinParameters, r Request) (Decision, error) {
	config := p.Config
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	raw, err := x.withKey(ctx, map[string]any{"input": map[string]string{"text": r.Text}, "metadata": map[string]string{}, "config": config}, func(h http.Header, key string) {
		h.Set("x-javelin-apikey", key)
		if p.Application != "" {
			h.Set("x-javelin-application", p.Application)
		}
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Assessments *[]map[string]struct {
			Reject *bool `json:"request_reject"`
		} `json:"assessments"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Assessments == nil {
		return Decision{}, errUnavailable
	}
	rejected := false
	for _, assessment := range *response.Assessments {
		if len(assessment) == 0 {
			return Decision{}, errUnavailable
		}
		for _, result := range assessment {
			if result.Reject == nil {
				return Decision{}, errUnavailable
			}
			rejected = rejected || *result.Reject
		}
	}
	return flagIf(rejected, "javelin.policy"), nil
}

type enkryptParameters struct {
	PolicyName string `json:"policy_name,omitempty"`
}

func (p enkryptParameters) valid() bool { return label(p.PolicyName) }

func enkrypt(ctx context.Context, x exchange, p enkryptParameters, r Request) (Decision, error) {
	raw, err := x.withKey(ctx, map[string]string{"text": r.Text}, func(h http.Header, key string) {
		h.Set("apikey", key)
		if p.PolicyName != "" {
			h.Set("x-enkrypt-policy", p.PolicyName)
		}
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Summary map[string]json.RawMessage `json:"summary"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Summary) == 0 {
		return Decision{}, errUnavailable
	}
	detected := false
	for key, value := range response.Summary {
		found, err := enkryptDetected(key, value)
		if err != nil {
			return Decision{}, err
		}
		detected = detected || found
	}
	return flagIf(detected, "enkryptai.policy"), nil
}

func enkryptDetected(key string, value json.RawMessage) (bool, error) {
	if key == "toxicity" {
		var detections []json.RawMessage
		if json.Unmarshal(value, &detections) != nil || string(value) == "null" {
			return false, errUnavailable
		}
		return len(detections) > 0, nil
	}
	var flagged *int
	if json.Unmarshal(value, &flagged) != nil || flagged == nil || (*flagged != 0 && *flagged != 1) {
		return false, errUnavailable
	}
	return *flagged == 1, nil
}

type ibmParameters struct {
	DetectorID     string          `json:"detector_id"`
	Orchestrator   bool            `json:"orchestrator,omitempty"`
	ScoreThreshold *float64        `json:"score_threshold,omitempty"`
	DetectorParams json.RawMessage `json:"detector_params,omitempty"`
}

func (p ibmParameters) valid() bool {
	if p.DetectorID == "" || !label(p.DetectorID) || (p.ScoreThreshold != nil && !unit(*p.ScoreThreshold)) {
		return false
	}
	if len(p.DetectorParams) == 0 {
		return true
	}
	var params map[string]json.RawMessage
	return json.Unmarshal(p.DetectorParams, &params) == nil && params != nil
}

type ibmDetection struct {
	Score     *float64 `json:"score"`
	Type      string   `json:"detection_type"`
	Detection string   `json:"detection"`
}

func ibm(ctx context.Context, x exchange, p ibmParameters, r Request) (Decision, error) {
	params := p.DetectorParams
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	body := map[string]any{"contents": []string{r.Text}, "detector_params": params}
	if p.Orchestrator {
		body = map[string]any{"content": r.Text, "detectors": map[string]any{p.DetectorID: params}}
	}
	raw, err := x.withKey(ctx, body, func(h http.Header, key string) {
		h.Set("Authorization", "Bearer "+key)
		if !p.Orchestrator {
			h.Set("detector-id", p.DetectorID)
		}
	})
	if err != nil {
		return Decision{}, err
	}
	detections, err := ibmDetections(raw, p.Orchestrator)
	if err != nil {
		return Decision{}, err
	}
	detected := false
	for _, d := range detections {
		if d.Score == nil || !unit(*d.Score) || d.Type == "" || d.Detection == "" {
			return Decision{}, errUnavailable
		}
		detected = detected || p.ScoreThreshold == nil || *d.Score >= *p.ScoreThreshold
	}
	return flagIf(detected, "ibm_guardrails.policy"), nil
}

func ibmDetections(raw []byte, orchestrator bool) ([]ibmDetection, error) {
	if orchestrator {
		var response struct {
			Detections *[]ibmDetection `json:"detections"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Detections == nil {
			return nil, errUnavailable
		}
		return *response.Detections, nil
	}
	var response [][]ibmDetection
	if json.Unmarshal(raw, &response) != nil || len(response) != 1 || response[0] == nil {
		return nil, errUnavailable
	}
	return response[0], nil
}

type ovalixParameters struct {
	ApplicationID string `json:"application_id"`
	CheckpointID  string `json:"checkpoint_id"`
	Actor         string `json:"actor"`
}

func (p ovalixParameters) valid() bool {
	return required(p.ApplicationID) && required(p.CheckpointID) && required(p.Actor)
}

func ovalix(ctx context.Context, x exchange, p ovalixParameters, r Request) (Decision, error) {
	body := map[string]any{"application_id": p.ApplicationID, "checkpoint_id": p.CheckpointID, "actor": p.Actor, "session_id": uuid.NewString(), "data_type": "TEXT", "data": map[string]string{"content": r.Text}}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action   string   `json:"action_type"`
		Modified *content `json:"modified_data"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	modified := response.Modified != nil && response.Modified.Content != nil
	switch {
	case response.Action == "block":
		return flag("ovalix.policy"), nil
	case response.Action != "allow" && response.Action != "anonymize":
		return Decision{}, errUnavailable
	case modified && (response.Action == "anonymize" || *response.Modified.Content != r.Text):
		return replace(r, *response.Modified.Content, "ovalix.policy")
	case response.Action == "anonymize":
		return Decision{}, errUnavailable
	default:
		return Decision{}, nil
	}
}

func vigil(ctx context.Context, x exchange, r Request) (Decision, error) {
	body := map[string]any{"text": r.Text, "source": pick(r, "user_input", "model_output"), "mode": "full", "metadata": map[string]any{}}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Decision  string  `json:"decision"`
		Sanitized *string `json:"sanitizedText"`
		Output    *string `json:"outputText"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Decision {
	case "ALLOWED":
		return Decision{}, nil
	case "BLOCKED":
		return flag("vigil_guard.policy"), nil
	case "SANITIZED":
		sanitized := cmp.Or(response.Sanitized, response.Output)
		if sanitized == nil {
			return Decision{}, errUnavailable
		}
		return replace(r, *sanitized, "vigil_guard.policy")
	default:
		return Decision{}, errUnavailable
	}
}

type repelloParameters struct {
	AssetID string `json:"asset_id"`
}

func (p repelloParameters) valid() bool { return required(p.AssetID) }

func repello(ctx context.Context, x exchange, p repelloParameters, r Request) (Decision, error) {
	body := map[string]any{"asset_id": p.AssetID, "scan_data": map[string]string{pick(r, "prompt", "response"): r.Text}}
	raw, err := x.keyed(ctx, body, "X-API-Key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Verdict {
	case "passed":
		return Decision{}, nil
	case "blocked", "flagged":
		return flag("repelloai.policy"), nil
	default:
		return Decision{}, errUnavailable
	}
}

type compresrParameters struct {
	Query                  string  `json:"query"`
	Model                  string  `json:"compression_model_name,omitempty"`
	TargetCompressionRatio float64 `json:"target_compression_ratio,omitempty"`
	Coarse                 bool    `json:"coarse,omitempty"`
	Dynamic                bool    `json:"dynamic,omitempty"`
}

func (p compresrParameters) valid() bool {
	return p.Query != "" && len(p.Query) <= 8192 && len(p.Model) <= 256 && unit(p.TargetCompressionRatio)
}

func compresr(ctx context.Context, x exchange, p compresrParameters, r Request) (Decision, error) {
	ratio := p.TargetCompressionRatio
	if ratio == 0 {
		ratio = 0.5
	}
	body := map[string]any{"context": r.Text, "query": p.Query, "compression_model_name": cmp.Or(p.Model, "latte_v2"), "target_compression_ratio": ratio, "coarse": p.Coarse, "dynamic": p.Dynamic, "source": "integration:olp"}
	raw, err := x.keyed(ctx, body, "X-API-Key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Data *struct {
			Context *string `json:"compressed_context"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Data == nil {
		return Decision{}, errUnavailable
	}
	return compressed(r, response.Data.Context, "compresr.compression")
}

type moderationParameters struct {
	Model string `json:"model,omitempty"`
}

func (moderationParameters) valid() bool { return true }

func openAIModeration(ctx context.Context, x exchange, p moderationParameters, r Request) (Decision, error) {
	body := map[string]any{"input": r.Text}
	if p.Model != "" {
		body["model"] = p.Model
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Results []struct {
			Flagged *bool `json:"flagged"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Results) != 1 || response.Results[0].Flagged == nil {
		return Decision{}, errUnavailable
	}
	return flagIf(*response.Results[0].Flagged, "vendor.openai_moderation"), nil
}
