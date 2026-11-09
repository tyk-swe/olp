package vendor

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"unicode/utf8"

	"github.com/google/uuid"
)

type content struct {
	Content *string `json:"content"`
}

func replaceOnly(messages []content, r Request, category string) (Decision, error) {
	if len(messages) != 1 || messages[0].Content == nil {
		return Decision{}, errUnavailable
	}
	return replace(r, *messages[0].Content, category)
}

type pangeaParameters struct {
	InputRecipe  string `json:"input_recipe,omitempty"`
	OutputRecipe string `json:"output_recipe,omitempty"`
}

func (p pangeaParameters) valid() bool { return optional(p.InputRecipe) && optional(p.OutputRecipe) }

func pangea(ctx context.Context, x exchange, p pangeaParameters, r Request) (Decision, error) {
	input := map[string]any{"messages": messages(r)}
	if r.Output {
		input = map[string]any{"messages": []any{}, "choices": []any{map[string]any{"index": 0, "message": message(r)}}}
	}
	body := map[string]any{"debug": false, "input": input, "event_type": pick(r, "input", "output")}
	if recipe := pick(r, p.InputRecipe, p.OutputRecipe); recipe != "" {
		body["recipe"] = recipe
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Status string `json:"status"`
		Result *struct {
			Blocked     *bool `json:"blocked"`
			Transformed *bool `json:"transformed"`
			Output      struct {
				Messages []content `json:"messages"`
				Choices  []struct {
					Message content `json:"message"`
				} `json:"choices"`
			} `json:"output"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Result == nil || response.Result.Blocked == nil || (response.Status != "" && response.Status != "Success") {
		return Decision{}, errUnavailable
	}
	if *response.Result.Blocked {
		return flag("pangea.policy"), nil
	}
	if response.Result.Transformed == nil || !*response.Result.Transformed {
		return Decision{}, nil
	}
	if !r.Output {
		return replaceOnly(response.Result.Output.Messages, r, "pangea.policy")
	}
	choices := make([]content, 0, len(response.Result.Output.Choices))
	for _, choice := range response.Result.Output.Choices {
		choices = append(choices, choice.Message)
	}
	return replaceOnly(choices, r, "pangea.policy")
}

type crowdstrikeParameters struct {
	Model string `json:"model,omitempty"`
}

func (p crowdstrikeParameters) valid() bool { return optional(p.Model) }

func crowdstrike(ctx context.Context, x exchange, p crowdstrikeParameters, r Request) (Decision, error) {
	body := map[string]any{"guard_input": map[string]any{"messages": messages(r), "tools": []any{}}, "event_type": pick(r, "input", "output")}
	if p.Model != "" {
		body["model"] = p.Model
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Result *struct {
			Blocked     *bool `json:"blocked"`
			Transformed *bool `json:"transformed"`
			Output      *struct {
				Messages []content `json:"messages"`
			} `json:"guard_output"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Result == nil || response.Result.Blocked == nil {
		return Decision{}, errUnavailable
	}
	if *response.Result.Blocked {
		return flag("crowdstrike_aidr.policy"), nil
	}
	if response.Result.Transformed == nil || !*response.Result.Transformed {
		return Decision{}, nil
	}
	if response.Result.Output == nil {
		return Decision{}, errUnavailable
	}
	return replaceOnly(response.Result.Output.Messages, r, "crowdstrike_aidr.policy")
}

type lassoParameters struct {
	UserID string `json:"user_id,omitempty"`
	Mask   bool   `json:"mask,omitempty"`
}

func (p lassoParameters) valid() bool { return optional(p.UserID) }

func lasso(ctx context.Context, x exchange, p lassoParameters, r Request) (Decision, error) {
	session := uuid.NewString()
	body := map[string]any{"messages": messages(r), "messageType": pick(r, "PROMPT", "COMPLETION"), "source": map[string]string{"type": "olp"}, "sessionId": session}
	if p.UserID != "" {
		body["userId"] = p.UserID
	}
	raw, err := x.withKey(ctx, body, func(h http.Header, key string) {
		h.Set("lasso-api-key", key)
		h.Set("lasso-conversation-id", session)
		if p.UserID != "" {
			h.Set("lasso-user-id", p.UserID)
		}
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Violations *bool `json:"violations_detected"`
		Findings   map[string][]struct {
			Action string `json:"action"`
		} `json:"findings"`
		Messages []content `json:"messages"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Violations == nil {
		return Decision{}, errUnavailable
	}
	if !*response.Violations {
		return Decision{}, nil
	}
	if len(response.Findings) == 0 {
		return Decision{}, errUnavailable
	}
	masked, warned := false, false
	for _, findings := range response.Findings {
		for _, finding := range findings {
			switch finding.Action {
			case "BLOCK":
				return flag("lasso.policy"), nil
			case "AUTO_MASKING":
				masked = true
			case "WARN":
				warned = true
			default:
				return Decision{}, errUnavailable
			}
		}
	}
	if masked && p.Mask && !warned {
		return replaceOnly(response.Messages, r, "lasso.policy")
	}
	return flag("lasso.policy"), nil
}

type pillarParameters struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

func (p pillarParameters) valid() bool { return optional(p.Model) && optional(p.Provider) }

func pillar(ctx context.Context, x exchange, p pillarParameters, r Request) (Decision, error) {
	body := map[string]any{"messages": messages(r), "tools": []any{}, "metadata": map[string]string{"source": "olp"}, "model": p.Model, "provider": p.Provider}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Flagged  *bool     `json:"flagged"`
		Messages []content `json:"masked_session_messages"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Flagged == nil {
		return Decision{}, errUnavailable
	}
	if *response.Flagged && len(response.Messages) > 0 {
		return replaceOnly(response.Messages, r, "pillar.policy")
	}
	return flagIf(*response.Flagged, "pillar.policy"), nil
}

type nomaParameters struct {
	ApplicationID string `json:"application_id,omitempty"`
	GatewayName   string `json:"gateway_name,omitempty"`
}

func (p nomaParameters) valid() bool { return label(p.ApplicationID) && label(p.GatewayName) }

func (p nomaParameters) validV1() bool { return p.valid() && p.GatewayName == "" }

func noma(ctx context.Context, x exchange, p nomaParameters, r Request) (Decision, error) {
	role := string(r.Role)
	body := map[string]any{"input": []any{map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": "input_text", "text": r.Text}}}}, "x-noma-context": map[string]string{"applicationId": p.ApplicationID}}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Blocked *bool `json:"aggregatedScanResult"`
		Results []struct {
			Role    string                     `json:"role"`
			Results map[string]json.RawMessage `json:"results"`
		} `json:"scanResult"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Blocked == nil {
		return Decision{}, errUnavailable
	}
	for _, result := range response.Results {
		anonymized, ok := result.Results["anonymizedContent"]
		if result.Role != role || !ok {
			continue
		}
		var replacement struct {
			Text *string `json:"anonymized"`
		}
		if json.Unmarshal(anonymized, &replacement) != nil || replacement.Text == nil {
			return Decision{}, errUnavailable
		}
		if len(response.Results) != 1 {
			if *response.Blocked {
				return flag("noma.policy"), nil
			}
			return Decision{}, errUnavailable
		}
		if *response.Blocked {
			sensitiveOnly, err := nomaSensitiveOnly(result.Results)
			if err != nil {
				return Decision{}, err
			}
			if !sensitiveOnly {
				continue
			}
		}
		if *replacement.Text != r.Text {
			return replace(r, *replacement.Text, "noma.policy")
		}
	}
	return flagIf(*response.Blocked, "noma.policy"), nil
}

func nomaSensitiveOnly(results map[string]json.RawMessage) (bool, error) {
	sensitive, other := false, false
	for key, raw := range results {
		if key == "anonymizedContent" {
			continue
		}
		detections, err := nomaDetections(raw)
		if err != nil {
			return false, err
		}
		for _, detection := range detections {
			flagged, err := nomaFlagged(detection)
			if err != nil {
				return false, err
			}
			sensitive = sensitive || (flagged && (key == "sensitiveData" || key == "dataDetector"))
			other = other || (flagged && key != "sensitiveData" && key != "dataDetector")
		}
	}
	return sensitive && !other, nil
}

func nomaDetections(raw json.RawMessage) ([]json.RawMessage, error) {
	var group map[string]json.RawMessage
	if json.Unmarshal(raw, &group) != nil || group == nil {
		return nil, errUnavailable
	}
	if _, direct := group["result"]; direct {
		return []json.RawMessage{raw}, nil
	}
	return slices.Collect(maps.Values(group)), nil
}

func nomaFlagged(raw json.RawMessage) (bool, error) {
	var detection struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &detection) != nil {
		return false, errUnavailable
	}
	if len(detection.Result) == 0 {
		return false, nil
	}
	var flagged bool
	if json.Unmarshal(detection.Result, &flagged) == nil {
		return flagged, nil
	}
	var text string
	if json.Unmarshal(detection.Result, &text) == nil && (text == "true" || text == "false") {
		return text == "true", nil
	}
	return false, errUnavailable
}

type roleContent struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
}

func replaceMessage(messages *[]roleContent, r Request, category string) (Decision, error) {
	if messages == nil || len(*messages) != 1 || (*messages)[0].Content == nil || (*messages)[0].Role != string(r.Role) {
		return Decision{}, errUnavailable
	}
	return replace(r, *(*messages)[0].Content, category)
}

type promptSecurityParameters struct {
	User         string `json:"user,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

func (p promptSecurityParameters) valid() bool {
	return label(p.User) && len(p.SystemPrompt) <= 8192 && utf8.ValidString(p.SystemPrompt)
}

type promptSecurityResult struct {
	Action   string         `json:"action"`
	Text     *string        `json:"modified_text"`
	Messages *[]roleContent `json:"modified_messages"`
}

func promptSecurity(ctx context.Context, x exchange, p promptSecurityParameters, r Request) (Decision, error) {
	body := map[string]any{"user": p.User, "system_prompt": p.SystemPrompt, "messages": messages(r)}
	if r.Output {
		body = map[string]any{"user": p.User, "system_prompt": p.SystemPrompt, "response": r.Text}
	}
	raw, err := x.keyed(ctx, body, "APP-ID", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Result struct {
			Prompt   *promptSecurityResult `json:"prompt"`
			Response *promptSecurityResult `json:"response"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	result := response.Result.Prompt
	if r.Output {
		result = response.Result.Response
	}
	if result == nil {
		return Decision{}, errUnavailable
	}
	if result.Action != "modify" {
		return allowBlock(result.Action, "prompt_security.policy")
	}
	if !r.Output {
		return replaceMessage(result.Messages, r, "prompt_security.policy")
	}
	if result.Text == nil {
		return Decision{}, errUnavailable
	}
	return replace(r, *result.Text, "prompt_security.policy")
}
