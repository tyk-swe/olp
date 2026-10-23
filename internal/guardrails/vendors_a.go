package guardrails

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// These contracts follow the pinned vendor inventory. Endpoints are complete
// operation URLs, so credentials are never forwarded to a discovered location.
var namedVendorsA = map[string]namedVendor{
	"aporia":           {validate: aNoParameters, evaluate: aAporia},
	"dynamoai":         {validate: aValidateDynamo, evaluate: aDynamo},
	"guardrails_ai":    {validate: aNoParameters, evaluate: aGuardrailsAI, transforms: true},
	"lakera_v1":        {validate: aValidateLakera, evaluate: aLakera, phases: []string{Input, InputParallel}},
	"hiddenlayer":      {validate: aValidateHiddenlayer, evaluate: aHiddenlayer, transforms: true},
	"aim":              {validate: aNoParameters, evaluate: aAim, transforms: true},
	"cato_networks":    {validate: aNoParameters, evaluate: aCato, transforms: true},
	"pangea":           {validate: aValidatePangea, evaluate: aPangea, transforms: true},
	"crowdstrike_aidr": {validate: aValidateCrowdstrike, evaluate: aCrowdstrike, transforms: true},
	"lasso":            {validate: aValidateLasso, evaluate: aLasso, transforms: true},
	"pillar":           {validate: aValidatePillar, evaluate: aPillar, transforms: true},
	"grayswan":         {validate: aValidateGrayswan, evaluate: aGrayswan},
}

func aNoParameters(raw json.RawMessage) error { return decodeVendorParameters(raw, &struct{}{}) }

func aOutput(phase string) bool { return phase == Output || phase == OutputStream }
func aRole(phase string, s Segment) string {
	if aOutput(phase) {
		return "assistant"
	}
	switch s.Role {
	case "system", "user", "assistant":
		return s.Role
	}
	return "user"
}
func aMessages(phase string, s Segment) []map[string]string {
	return []map[string]string{{"role": aRole(phase, s), "content": s.Text}}
}
func aRead(raw []byte, target any) error {
	if json.Unmarshal(raw, target) != nil {
		return errEvaluation
	}
	return nil
}
func aBounded(value string) bool { return len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00") }
func aIDs(values []string) bool {
	if len(values) > 64 {
		return false
	}
	for _, v := range values {
		if v == "" || !aBounded(v) {
			return false
		}
	}
	return true
}

type aMessage struct {
	Content *string `json:"content"`
}

func aReplacement(messages []aMessage, s Segment, category string) (Verdict, error) {
	if len(messages) != 1 || messages[0].Content == nil {
		return Verdict{}, errEvaluation
	}
	return vendorReplacement(s, *messages[0].Content, category)
}

func aAporia(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	body := map[string]any{"messages": aMessages(phase, s), "validation_target": "prompt"}
	if aOutput(phase) {
		body = map[string]any{"response": s.Text, "validation_target": "response"}
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "X-APORIA-API-KEY", "")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Action string `json:"action"`
	}
	if aRead(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Action {
	case "passthrough":
		return vendorBoolean(false, "aporia.policy"), nil
	case "block":
		return vendorBoolean(true, "aporia.policy"), nil
	}
	// The reference contract does not define a typed replacement for modify or
	// rephrase. Treat these as evaluation failures instead of forwarding raw text.
	return Verdict{}, errEvaluation
}

type aDynamoParameters struct {
	ModelID   string   `json:"model_id,omitempty"`
	PolicyIDs []string `json:"policy_ids,omitempty"`
}

func aValidateDynamo(raw json.RawMessage) error {
	var p aDynamoParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.ModelID) || !aIDs(p.PolicyIDs) {
		return errEvaluation
	}
	return nil
}
func aDynamo(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aDynamoParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	body := map[string]any{"messages": aMessages(phase, s)}
	if p.ModelID != "" {
		body["modelId"] = p.ModelID
	}
	if len(p.PolicyIDs) > 0 {
		body["policyIds"] = p.PolicyIDs
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		FinalAction string `json:"finalAction"`
	}
	if aRead(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.FinalAction {
	case "NONE":
		return vendorBoolean(false, "dynamoai.policy"), nil
	case "BLOCK", "WARN":
		return vendorBoolean(true, "dynamoai.policy"), nil
	}
	return Verdict{}, errEvaluation
}

func aGuardrailsAI(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, _ string, s Segment) (Verdict, error) {
	raw, err := vendorJSON(ctx, client, c.Endpoint, map[string]string{"llmOutput": s.Text}, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Passed *bool   `json:"validationPassed"`
		Output *string `json:"validatedOutput"`
	}
	if aRead(raw, &response) != nil || response.Passed == nil {
		return Verdict{}, errEvaluation
	}
	if !*response.Passed {
		return vendorBoolean(true, "guardrails_ai.validation"), nil
	}
	if response.Output != nil && *response.Output != s.Text {
		return vendorReplacement(s, *response.Output, "guardrails_ai.validation")
	}
	return vendorBoolean(false, "guardrails_ai.validation"), nil
}

type aLakeraParameters struct {
	CategoryThresholds map[string]float64 `json:"category_thresholds,omitempty"`
}

func aValidateLakera(raw json.RawMessage) error {
	var p aLakeraParameters
	if decodeVendorParameters(raw, &p) != nil {
		return errEvaluation
	}
	for k, v := range p.CategoryThresholds {
		if (k != "jailbreak" && k != "prompt_injection") || math.IsNaN(v) || v < 0 || v > 1 {
			return errEvaluation
		}
	}
	return nil
}
func aLakera(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	if aOutput(phase) {
		return Verdict{}, errEvaluation
	}
	var p aLakeraParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, map[string]string{"input": s.Text}, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Results []struct {
			Flagged *bool               `json:"flagged"`
			Scores  map[string]*float64 `json:"category_scores"`
		} `json:"results"`
	}
	if aRead(raw, &response) != nil || len(response.Results) != 1 || response.Results[0].Flagged == nil {
		return Verdict{}, errEvaluation
	}
	blocked := *response.Results[0].Flagged
	if len(p.CategoryThresholds) > 0 {
		blocked = false
		for k, threshold := range p.CategoryThresholds {
			score, ok := response.Results[0].Scores[k]
			if !ok || score == nil || *score < 0 || *score > 1 {
				return Verdict{}, errEvaluation
			}
			blocked = blocked || *score >= threshold
		}
	}
	return vendorBoolean(blocked, "lakera.prompt_injection"), nil
}

type aHiddenlayerParameters struct {
	ProjectID string `json:"project_id,omitempty"`
	Model     string `json:"model,omitempty"`
}

func aValidateHiddenlayer(raw json.RawMessage) error {
	var p aHiddenlayerParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.ProjectID) || !aBounded(p.Model) {
		return errEvaluation
	}
	return nil
}
func aHiddenlayer(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aHiddenlayerParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	token := cred.APIKey
	if c.AuthEndpoint != "" {
		if cred.ClientID == "" || cred.ClientSecret == "" {
			return Verdict{}, errEvaluation
		}
		u, err := url.Parse(c.AuthEndpoint)
		if err != nil {
			return Verdict{}, errEvaluation
		}
		q := u.Query()
		q.Set("grant_type", "client_credentials")
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
		if err != nil {
			return Verdict{}, errEvaluation
		}
		req.SetBasicAuth(cred.ClientID, cred.ClientSecret)
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return Verdict{}, errEvaluation
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return Verdict{}, errEvaluation
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
		if err != nil || len(raw) > 1<<20 {
			return Verdict{}, errEvaluation
		}
		var auth struct {
			AccessToken string `json:"access_token"`
		}
		if aRead(raw, &auth) != nil || auth.AccessToken == "" || len(auth.AccessToken) > 64<<10 {
			return Verdict{}, errEvaluation
		}
		token = auth.AccessToken
	}
	side := "input"
	if aOutput(phase) {
		side = "output"
	}
	body := map[string]any{side: map[string]any{"messages": aMessages(phase, s)}, "metadata": map[string]string{"model": p.Model, "requester_id": "OLP"}}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(req *http.Request, _ []byte) error {
		if token == "" {
			return errEvaluation
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("hl-runtime-edge-provider", "olp")
		req.Header.Set("hl-runtime-edge-provider-version", "1")
		if p.ProjectID != "" {
			req.Header.Set("HL-Project-Id", p.ProjectID)
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Evaluation *struct {
			Action string `json:"action"`
		} `json:"evaluation"`
		Modified map[string]struct {
			Messages []aMessage `json:"messages"`
		} `json:"modified_data"`
	}
	if aRead(raw, &response) != nil || response.Evaluation == nil {
		return Verdict{}, errEvaluation
	}
	switch response.Evaluation.Action {
	case "Allow":
		return vendorBoolean(false, "hiddenlayer.policy"), nil
	case "Block":
		return vendorBoolean(true, "hiddenlayer.policy"), nil
	case "Redact":
		return aReplacement(response.Modified[side].Messages, s, "hiddenlayer.policy")
	}
	return Verdict{}, errEvaluation
}

func aAim(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	return aAnalyze(ctx, client, c, cred, phase, s, "aim")
}
func aCato(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	return aAnalyze(ctx, client, c, cred, phase, s, "cato")
}
func aAnalyze(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment, vendor string) (Verdict, error) {
	raw, err := externalJSON(ctx, client, c.Endpoint, map[string]any{"messages": aMessages(phase, s)}, func(req *http.Request, _ []byte) error {
		if cred.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
		hook := "pre_call"
		if aOutput(phase) {
			hook = "post_call"
		}
		req.Header.Set("x-"+vendor+"-litellm-hook", hook)
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Required *struct {
			Action *string `json:"action_type"`
		} `json:"required_action"`
		Analysis json.RawMessage `json:"analysis_result"`
		Redacted *struct {
			Messages []aMessage `json:"all_redacted_messages"`
		} `json:"redacted_chat"`
	}
	if aRead(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	if response.Required == nil || response.Required.Action == nil {
		var analysis map[string]json.RawMessage
		if json.Unmarshal(response.Analysis, &analysis) != nil || analysis == nil {
			return Verdict{}, errEvaluation
		}
		return vendorBoolean(false, vendor+".policy"), nil
	}
	switch *response.Required.Action {
	case "monitor_action":
		return vendorBoolean(true, vendor+".policy"), nil
	case "block_action":
		return vendorBoolean(true, vendor+".policy"), nil
	case "anonymize_action":
		if response.Redacted != nil {
			return aReplacement(response.Redacted.Messages, s, vendor+".policy")
		}
	}
	return Verdict{}, errEvaluation
}

type aPangeaParameters struct {
	InputRecipe  string `json:"input_recipe,omitempty"`
	OutputRecipe string `json:"output_recipe,omitempty"`
}

func aValidatePangea(raw json.RawMessage) error {
	var p aPangeaParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.InputRecipe) || !aBounded(p.OutputRecipe) {
		return errEvaluation
	}
	return nil
}
func aPangea(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aPangeaParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	kind, recipe := "input", p.InputRecipe
	input := map[string]any{"messages": aMessages(phase, s)}
	if aOutput(phase) {
		kind, recipe = "output", p.OutputRecipe
		input = map[string]any{"messages": []any{}, "choices": []any{map[string]any{"index": 0, "message": aMessages(phase, s)[0]}}}
	}
	body := map[string]any{"debug": false, "input": input, "event_type": kind}
	if recipe != "" {
		body["recipe"] = recipe
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Status string `json:"status"`
		Result *struct {
			Blocked     *bool `json:"blocked"`
			Transformed *bool `json:"transformed"`
			Output      struct {
				Messages []aMessage `json:"messages"`
				Choices  []struct {
					Message aMessage `json:"message"`
				} `json:"choices"`
			} `json:"output"`
		} `json:"result"`
	}
	if aRead(raw, &response) != nil || response.Result == nil || response.Result.Blocked == nil || (response.Status != "" && response.Status != "Success") {
		return Verdict{}, errEvaluation
	}
	if *response.Result.Blocked {
		return vendorBoolean(true, "pangea.policy"), nil
	}
	if response.Result.Transformed != nil && *response.Result.Transformed {
		messages := response.Result.Output.Messages
		if aOutput(phase) {
			messages = nil
			for _, ch := range response.Result.Output.Choices {
				messages = append(messages, ch.Message)
			}
		}
		return aReplacement(messages, s, "pangea.policy")
	}
	return vendorBoolean(false, "pangea.policy"), nil
}

type aCrowdstrikeParameters struct {
	Model string `json:"model,omitempty"`
}

func aValidateCrowdstrike(raw json.RawMessage) error {
	var p aCrowdstrikeParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.Model) {
		return errEvaluation
	}
	return nil
}
func aCrowdstrike(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aCrowdstrikeParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	kind := "input"
	if aOutput(phase) {
		kind = "output"
	}
	body := map[string]any{"guard_input": map[string]any{"messages": aMessages(phase, s), "tools": []any{}}, "event_type": kind}
	if p.Model != "" {
		body["model"] = p.Model
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Result *struct {
			Blocked     *bool `json:"blocked"`
			Transformed *bool `json:"transformed"`
			Output      *struct {
				Messages []aMessage `json:"messages"`
			} `json:"guard_output"`
		} `json:"result"`
	}
	if aRead(raw, &response) != nil || response.Result == nil || response.Result.Blocked == nil {
		return Verdict{}, errEvaluation
	}
	if *response.Result.Blocked {
		return vendorBoolean(true, "crowdstrike_aidr.policy"), nil
	}
	if response.Result.Transformed != nil && *response.Result.Transformed {
		if response.Result.Output == nil {
			return Verdict{}, errEvaluation
		}
		return aReplacement(response.Result.Output.Messages, s, "crowdstrike_aidr.policy")
	}
	return vendorBoolean(false, "crowdstrike_aidr.policy"), nil
}

type aLassoParameters struct {
	UserID string `json:"user_id,omitempty"`
	Mask   bool   `json:"mask,omitempty"`
}

func aValidateLasso(raw json.RawMessage) error {
	var p aLassoParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.UserID) {
		return errEvaluation
	}
	return nil
}
func aLasso(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aLassoParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	// A fresh identifier avoids joining unrelated gateway requests in Lasso.
	session := uuid.NewString()
	kind := "PROMPT"
	if aOutput(phase) {
		kind = "COMPLETION"
	}
	body := map[string]any{"messages": aMessages(phase, s), "messageType": kind, "source": map[string]string{"type": "olp"}, "sessionId": session}
	if p.UserID != "" {
		body["userId"] = p.UserID
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(req *http.Request, _ []byte) error {
		if cred.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("lasso-api-key", cred.APIKey)
		req.Header.Set("lasso-conversation-id", session)
		if p.UserID != "" {
			req.Header.Set("lasso-user-id", p.UserID)
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Violations *bool `json:"violations_detected"`
		Findings   map[string][]struct {
			Action string `json:"action"`
		} `json:"findings"`
		Messages []aMessage `json:"messages"`
	}
	if aRead(raw, &response) != nil || response.Violations == nil {
		return Verdict{}, errEvaluation
	}
	if !*response.Violations {
		return vendorBoolean(false, "lasso.policy"), nil
	}
	if len(response.Findings) == 0 {
		return Verdict{}, errEvaluation
	}
	needsMask, unrepairable := false, false
	for _, findings := range response.Findings {
		for _, finding := range findings {
			switch finding.Action {
			case "BLOCK":
				return vendorBoolean(true, "lasso.policy"), nil
			case "AUTO_MASKING":
				needsMask = true
			case "WARN":
				unrepairable = true
			default:
				return Verdict{}, errEvaluation
			}
		}
	}
	if needsMask && p.Mask && !unrepairable {
		return aReplacement(response.Messages, s, "lasso.policy")
	}
	return vendorBoolean(true, "lasso.policy"), nil
}

type aPillarParameters struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

func aValidatePillar(raw json.RawMessage) error {
	var p aPillarParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.Model) || !aBounded(p.Provider) {
		return errEvaluation
	}
	return nil
}
func aPillar(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aPillarParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	body := map[string]any{"messages": aMessages(phase, s), "tools": []any{}, "metadata": map[string]string{"source": "olp"}, "model": p.Model, "provider": p.Provider}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Flagged  *bool      `json:"flagged"`
		Messages []aMessage `json:"masked_session_messages"`
	}
	if aRead(raw, &response) != nil || response.Flagged == nil {
		return Verdict{}, errEvaluation
	}
	if *response.Flagged && len(response.Messages) > 0 {
		return aReplacement(response.Messages, s, "pillar.policy")
	}
	return vendorBoolean(*response.Flagged, "pillar.policy"), nil
}

type aGrayswanParameters struct {
	Threshold     *float64          `json:"violation_threshold,omitempty"`
	PolicyID      string            `json:"policy_id,omitempty"`
	ReasoningMode string            `json:"reasoning_mode,omitempty"`
	Categories    map[string]string `json:"categories,omitempty"`
}

func aValidateGrayswan(raw json.RawMessage) error {
	var p aGrayswanParameters
	if decodeVendorParameters(raw, &p) != nil || !aBounded(p.PolicyID) || len(p.Categories) > 32 {
		return errEvaluation
	}
	if p.Threshold != nil && (math.IsNaN(*p.Threshold) || *p.Threshold < 0 || *p.Threshold > 1) {
		return errEvaluation
	}
	switch p.ReasoningMode {
	case "", "off", "hybrid", "thinking":
	default:
		return errEvaluation
	}
	for k, v := range p.Categories {
		if !aBounded(k) || len(v) > 4096 {
			return errEvaluation
		}
	}
	return nil
}
func aGrayswan(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p aGrayswanParameters
	if decodeVendorParameters(c.Parameters, &p) != nil {
		return Verdict{}, errEvaluation
	}
	threshold := 0.5
	if p.Threshold != nil {
		threshold = *p.Threshold
	}
	body := map[string]any{"messages": aMessages(phase, s)}
	if p.PolicyID != "" {
		body["policy_id"] = p.PolicyID
	}
	if p.ReasoningMode != "" {
		body["reasoning_mode"] = p.ReasoningMode
	}
	if len(p.Categories) > 0 {
		body["categories"] = p.Categories
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(req *http.Request, _ []byte) error {
		if cred.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
		req.Header.Set("grayswan-api-key", cred.APIKey)
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Violation *float64 `json:"violation"`
	}
	if aRead(raw, &response) != nil || response.Violation == nil || *response.Violation < 0 || *response.Violation > 1 {
		return Verdict{}, errEvaluation
	}
	return vendorBoolean(*response.Violation >= threshold, "grayswan.policy"), nil
}
