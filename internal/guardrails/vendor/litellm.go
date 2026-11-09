package vendor

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

func nomaV2(ctx context.Context, x exchange, p nomaParameters, r Request) (Decision, error) {
	body := map[string]any{"inputs": map[string]any{"texts": []string{r.Text}}, "request_data": map[string]any{}, "input_type": direction(r), "monitor_mode": false}
	if p.ApplicationID != "" {
		body["application_id"] = p.ApplicationID
	}
	if p.GatewayName != "" {
		body["gateway_name"] = p.GatewayName
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action string     `json:"action"`
		Texts  *[]*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Action {
	case "NONE":
		return Decision{}, nil
	case "BLOCKED":
		return flag("noma_v2.policy"), nil
	case "GUARDRAIL_INTERVENED":
		if response.Texts == nil || len(*response.Texts) != 1 || (*response.Texts)[0] == nil {
			return Decision{}, errUnavailable
		}
		return replace(r, *(*response.Texts)[0], "noma_v2.policy")
	default:
		return Decision{}, errUnavailable
	}
}

func onyx(ctx context.Context, x exchange, r Request) (Decision, error) {
	key, err := x.apiKey()
	if err != nil {
		return Decision{}, err
	}
	payload := map[string]any{"messages": messages(r)}
	if r.Output {
		payload = map[string]any{"object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": string(Assistant), "content": r.Text}, "finish_reason": "stop"}}}
	}
	body := map[string]any{"payload": payload, "input_type": direction(r), "conversation_id": uuid.NewString()}
	raw, err := x.path("/guard/evaluate/v1/"+url.PathEscape(key)+"/litellm").post(ctx, body, func(*http.Request, []byte) error { return nil })
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Allowed *bool `json:"allowed"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Allowed == nil {
		return Decision{}, errUnavailable
	}
	return flagIf(!*response.Allowed, "onyx.policy"), nil
}

type deepkeepParameters struct {
	FirewallID string `json:"firewall_id"`
}

func (p deepkeepParameters) valid() bool { return required(p.FirewallID) }

func deepkeep(ctx context.Context, x exchange, p deepkeepParameters, r Request) (Decision, error) {
	return litellmTexts(ctx, x, r, "deepkeep", map[string]string{"firewall_id": p.FirewallID}, func(http.Header) {})
}

type qostodianParameters struct {
	Source    string `json:"source,omitempty"`
	Container string `json:"container,omitempty"`
	Identity  string `json:"identity,omitempty"`
}

func (p qostodianParameters) valid() bool {
	return optional(p.Source) && optional(p.Container) && optional(p.Identity)
}

func qostodian(ctx context.Context, x exchange, p qostodianParameters, r Request) (Decision, error) {
	return litellmTexts(ctx, x, r, "qostodian_nexus", map[string]any{}, func(h http.Header) {
		h.Set("x-qostodian-nexus-identifiers-trace", uuid.NewString())
		for name, value := range map[string]string{"source": p.Source, "container": p.Container, "identity": p.Identity} {
			if value != "" {
				h.Set("x-qostodian-nexus-identifiers-"+name, value)
			}
		}
	})
}

func litellmTexts(ctx context.Context, x exchange, r Request, name string, params any, headers func(http.Header)) (Decision, error) {
	body := map[string]any{"texts": []string{r.Text}, "input_type": direction(r), "request_data": map[string]any{}, "litellm_call_id": nil, "litellm_trace_id": nil, "litellm_version": "1.105.0", "images": nil, "tools": nil, "structured_messages": nil, "tool_calls": nil, "model": nil, "additional_provider_specific_params": params}
	raw, err := x.withKey(ctx, body, func(h http.Header, key string) {
		h.Set("X-API-Key", key)
		headers(h)
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action string     `json:"action"`
		Texts  *[]*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	category := name + ".policy"
	intervened := response.Action == "GUARDRAIL_INTERVENED"
	switch {
	case response.Action == "BLOCKED":
		return flag(category), nil
	case response.Action != "NONE" && !intervened:
		return Decision{}, errUnavailable
	case response.Texts == nil && intervened:
		return Decision{}, errUnavailable
	case response.Texts == nil:
		return Decision{}, nil
	case len(*response.Texts) != 1 || (*response.Texts)[0] == nil:
		return Decision{}, errUnavailable
	case *(*response.Texts)[0] != r.Text:
		return replace(r, *(*response.Texts)[0], category)
	default:
		return flagIf(intervened, category), nil
	}
}

type straikerParameters struct {
	Source string `json:"source,omitempty"`
}

func (straikerParameters) valid() bool { return true }

func straiker(ctx context.Context, x exchange, p straikerParameters, r Request) (Decision, error) {
	texts := map[string]any{"texts": []string{r.Text}, "images": []string{}}
	body := map[string]any{"schema_version": "1", "event": map[string]string{"type": pick(r, "pre_call", "post_call"), "id": uuid.NewString()}, "request": texts, "context": map[string]string{"call_surface": "completion"}, "identity": map[string]any{}, "application": map[string]string{"source": cmp.Or(p.Source, "olp")}}
	if r.Output {
		body["request"] = map[string]any{"texts": []string{}}
		body["response"] = texts
	}
	raw, err := x.withKey(ctx, body, func(h http.Header, key string) {
		h.Set("Authorization", "Bearer "+key)
		h.Set("X-Straiker-Webhook-Format", "litellm")
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action string     `json:"action"`
		Texts  *[]*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Action {
	case "NONE":
		return Decision{}, nil
	case "BLOCKED":
		return flag("straiker.policy"), nil
	case "GUARDRAIL_INTERVENED":
		if response.Texts == nil || len(*response.Texts) != 1 || (*response.Texts)[0] == nil {
			return Decision{}, errUnavailable
		}
		return replace(r, *(*response.Texts)[0], "straiker.policy")
	default:
		return Decision{}, errUnavailable
	}
}

func alice(ctx context.Context, x exchange, r Request) (Decision, error) {
	body := map[string]any{"input_type": direction(r), "inputs": map[string]any{"texts": []string{r.Text}}, "request_data": map[string]any{}}
	raw, err := x.keyed(ctx, body, "af-api-key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Verdict      string `json:"verdict"`
		Replacements []struct {
			Index *int    `json:"index"`
			Text  *string `json:"text"`
		} `json:"replacements"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Verdict {
	case "ALLOW":
		return Decision{}, nil
	case "DETECT", "BLOCK":
		return flag("alice.policy"), nil
	case "MASK":
		if len(response.Replacements) != 1 || response.Replacements[0].Index == nil || *response.Replacements[0].Index != 0 || response.Replacements[0].Text == nil {
			return Decision{}, errUnavailable
		}
		return replace(r, *response.Replacements[0].Text, "alice.policy")
	default:
		return Decision{}, errUnavailable
	}
}

func genericGuardrail(ctx context.Context, x exchange, r Request) (Decision, error) {
	body := map[string]any{"texts": []string{r.Text}, "input_type": direction(r), "additional_provider_specific_params": map[string]any{}}
	raw, err := x.keyed(ctx, body, "x-api-key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action string    `json:"action"`
		Texts  []*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	const category = "vendor.generic_guardrail_api"
	intervened := response.Action == "GUARDRAIL_INTERVENED"
	switch {
	case response.Action == "BLOCKED":
		return flag(category), nil
	case response.Action != "NONE" && !intervened:
		return Decision{}, errUnavailable
	case response.Action == "NONE" && len(response.Texts) == 0:
		return Decision{}, nil
	case len(response.Texts) != 1 || response.Texts[0] == nil:
		return Decision{}, errUnavailable
	case *response.Texts[0] == r.Text:
		return flagIf(intervened, category), nil
	default:
		return replace(r, *response.Texts[0], category)
	}
}
