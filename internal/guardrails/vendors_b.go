package guardrails

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// These profiles follow the native APIs in the pinned integration inventory.
// Endpoint is the complete operation URL, except for Onyx's credential path.
var namedVendorsB = map[string]namedVendor{
	"panw_prisma_airs": {validate: validatePANW, evaluate: evaluatePANW, transforms: true},
	"cisco_ai_defense": {validate: validateCisco, evaluate: evaluateCisco, transforms: true},
	"noma":             {validate: validateNomaV1, evaluate: evaluateNoma, transforms: true},
	"noma_v2":          {validate: validateNoma, evaluate: evaluateNomaV2, transforms: true},
	"zscaler_ai_guard": {validate: validateZscaler, evaluate: evaluateZscaler},
	"javelin":          {validate: validateJavelin, evaluate: evaluateJavelin, phases: []string{Input, InputParallel}},
	"enkryptai":        {validate: validateEnkrypt, evaluate: evaluateEnkrypt},
	"ibm_guardrails":   {validate: validateIBM, evaluate: evaluateIBM},
	"onyx":             {validate: validateBEmpty, evaluate: evaluateOnyx},
	"promptguard":      {validate: validateBEmpty, evaluate: evaluatePromptGuard, transforms: true},
	"xecguard":         {validate: validateXecguard, evaluate: evaluateXecguard},
	"prompt_security":  {validate: validatePromptSecurity, evaluate: evaluatePromptSecurity, transforms: true},
}

func validateBEmpty(raw json.RawMessage) error { return decodeVendorParameters(raw, &struct{}{}) }
func bString(value string) bool {
	return len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}
func bOutput(phase string) bool { return phase == Output || phase == OutputStream }
func bDirection(phase string) string {
	if bOutput(phase) {
		return "response"
	}
	return "request"
}
func bRole(phase string, s Segment) string {
	if bOutput(phase) {
		return "assistant"
	}
	if s.Role == "system" || s.Role == "developer" {
		return "system"
	}
	return "user"
}
func bMessages(phase string, s Segment) []map[string]string {
	return []map[string]string{{"role": bRole(phase, s), "content": s.Text}}
}
func bAction(action, category string) (Verdict, error) {
	switch action {
	case "allow":
		return vendorBoolean(false, category), nil
	case "block":
		return vendorBoolean(true, category), nil
	default:
		return Verdict{}, errEvaluation
	}
}

type panwParameters struct {
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
}

func validatePANW(raw json.RawMessage) error {
	var p panwParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.ProfileID) || !bString(p.ProfileName) {
		return errEvaluation
	}
	return nil
}
func evaluatePANW(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p panwParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	key := "prompt"
	if bOutput(phase) {
		key = "response"
	}
	body := map[string]any{"contents": []map[string]string{{key: s.Text}}, "metadata": map[string]any{"is_response": bOutput(phase)}}
	if p.ProfileID != "" || p.ProfileName != "" {
		profile := map[string]string{}
		if p.ProfileID != "" {
			profile["profile_id"] = p.ProfileID
		}
		if p.ProfileName != "" {
			profile["profile_name"] = p.ProfileName
		}
		body["ai_profile"] = profile
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "x-pan-token", "")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Action string `json:"action"`
		Prompt *struct {
			Data *string `json:"data"`
		} `json:"prompt_masked_data"`
		Response *struct {
			Data *string `json:"data"`
		} `json:"response_masked_data"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Error) > 0 && string(out.Error) != "null" {
		return Verdict{}, errEvaluation
	}
	v, err := bAction(out.Action, "panw_prisma_airs.policy")
	if err != nil {
		return Verdict{}, err
	}
	replacement := out.Prompt
	if bOutput(phase) {
		replacement = out.Response
	}
	if replacement != nil {
		if replacement.Data == nil {
			return Verdict{}, errEvaluation
		}
		if *replacement.Data != s.Text {
			return vendorReplacement(s, *replacement.Data, "panw_prisma_airs.policy")
		}
	}
	return v, nil
}

type ciscoParameters struct {
	ProfileID       string `json:"integration_profile_id,omitempty"`
	ProfileVersion  string `json:"integration_profile_version,omitempty"`
	TenantID        string `json:"integration_tenant_id,omitempty"`
	IntegrationType string `json:"integration_type,omitempty"`
	EnabledRules    []struct {
		RuleName       string   `json:"rule_name"`
		RuleID         string   `json:"rule_id,omitempty"`
		EntityTypes    []string `json:"entity_types,omitempty"`
		Classification string   `json:"classification,omitempty"`
	} `json:"enabled_rules,omitempty"`
}

func validateCisco(raw json.RawMessage) error {
	var p ciscoParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.ProfileID) || !bString(p.ProfileVersion) || !bString(p.TenantID) || !bString(p.IntegrationType) || len(p.EnabledRules) > 64 {
		return errEvaluation
	}
	for _, r := range p.EnabledRules {
		if !bString(r.RuleName) || (r.RuleName == "" && r.RuleID == "") || !bString(r.RuleID) || !bString(r.Classification) || len(r.EntityTypes) > 64 {
			return errEvaluation
		}
		for _, entity := range r.EntityTypes {
			if entity == "" || !bString(entity) {
				return errEvaluation
			}
		}
	}
	return nil
}
func evaluateCisco(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p ciscoParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, map[string]any{"messages": bMessages(phase, s), "metadata": map[string]string{}, "config": p}, cred, "X-Cisco-AI-Defense-API-Key", "")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Action         string          `json:"action"`
		Safe           *bool           `json:"is_safe"`
		Sanitized      *string         `json:"sanitized_text"`
		CamelSanitized *string         `json:"sanitizedText"`
		Error          json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Error) > 0 && string(out.Error) != "null" {
		return Verdict{}, errEvaluation
	}
	action := strings.ToLower(strings.TrimSpace(out.Action))
	switch action {
	case "deny", "denied", "block", "blocked", "reject", "rejected", "unsafe", "malicious":
		return vendorBoolean(true, "cisco_ai_defense.policy"), nil
	case "allow", "allowed", "safe", "ok":
		return vendorBoolean(false, "cisco_ai_defense.policy"), nil
	case "redact", "redacted", "sanitize", "sanitized", "mask":
		replacement := out.Sanitized
		if replacement == nil {
			replacement = out.CamelSanitized
		}
		if replacement == nil {
			return Verdict{}, errEvaluation
		}
		return vendorReplacement(s, *replacement, "cisco_ai_defense.policy")
	case "":
		if out.Safe != nil {
			return vendorBoolean(!*out.Safe, "cisco_ai_defense.policy"), nil
		}
	}
	return Verdict{}, errEvaluation
}

type nomaParameters struct {
	ApplicationID string `json:"application_id,omitempty"`
	GatewayName   string `json:"gateway_name,omitempty"`
}

func validateNoma(raw json.RawMessage) error {
	var p nomaParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.ApplicationID) || !bString(p.GatewayName) {
		return errEvaluation
	}
	return nil
}
func validateNomaV1(raw json.RawMessage) error {
	if err := validateNoma(raw); err != nil {
		return err
	}
	var p nomaParameters
	if decodeVendorParameters(raw, &p) != nil || p.GatewayName != "" {
		return errEvaluation
	}
	return nil
}

// A sensitive-data-only intervention may be anonymized. Other detections keep
// the block even when the service also supplies an anonymized alternative.
func nomaSensitiveOnly(results map[string]json.RawMessage) (bool, error) {
	sensitive, other := false, false
	for key, raw := range results {
		if key == "anonymizedContent" {
			continue
		}
		var group map[string]json.RawMessage
		if json.Unmarshal(raw, &group) != nil || group == nil {
			return false, errEvaluation
		}
		check := func(value json.RawMessage) (bool, error) {
			var detection struct {
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(value, &detection) != nil {
				return false, errEvaluation
			}
			if len(detection.Result) == 0 {
				return false, nil
			}
			var flag bool
			if json.Unmarshal(detection.Result, &flag) == nil {
				return flag, nil
			}
			var text string
			if json.Unmarshal(detection.Result, &text) == nil && (text == "true" || text == "false") {
				return text == "true", nil
			}
			return false, errEvaluation
		}
		values := []json.RawMessage{raw}
		if _, direct := group["result"]; !direct {
			values = values[:0]
			for _, value := range group {
				values = append(values, value)
			}
		}
		for _, value := range values {
			flag, err := check(value)
			if err != nil {
				return false, err
			}
			if flag {
				if key == "sensitiveData" || key == "dataDetector" {
					sensitive = true
				} else {
					other = true
				}
			}
		}
	}
	return sensitive && !other, nil
}

func evaluateNoma(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p nomaParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	role := bRole(phase, s)
	body := map[string]any{"input": []any{map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": "input_text", "text": s.Text}}}}, "x-noma-context": map[string]string{"applicationId": p.ApplicationID}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Blocked *bool `json:"aggregatedScanResult"`
		Results []struct {
			Role    string                     `json:"role"`
			Results map[string]json.RawMessage `json:"results"`
		} `json:"scanResult"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Blocked == nil {
		return Verdict{}, errEvaluation
	}
	for _, result := range out.Results {
		if result.Role != role {
			continue
		}
		anonymized, ok := result.Results["anonymizedContent"]
		if !ok {
			continue
		}
		var replacement struct {
			Text *string `json:"anonymized"`
		}
		if json.Unmarshal(anonymized, &replacement) != nil || replacement.Text == nil {
			return Verdict{}, errEvaluation
		}
		if len(out.Results) != 1 {
			// The aggregate verdict can include another message's violation;
			// its block cannot be cleared by this message's anonymization.
			if *out.Blocked {
				return vendorBoolean(true, "noma.policy"), nil
			}
			return Verdict{}, errEvaluation
		}
		if *out.Blocked {
			onlySensitive, err := nomaSensitiveOnly(result.Results)
			if err != nil {
				return Verdict{}, err
			}
			if !onlySensitive {
				continue
			}
		}
		if *replacement.Text != s.Text {
			return vendorReplacement(s, *replacement.Text, "noma.policy")
		}
	}
	return vendorBoolean(*out.Blocked, "noma.policy"), nil
}

func evaluateNomaV2(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p nomaParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	body := map[string]any{"inputs": map[string]any{"texts": []string{s.Text}}, "request_data": map[string]any{}, "input_type": bDirection(phase), "monitor_mode": false}
	if p.ApplicationID != "" {
		body["application_id"] = p.ApplicationID
	}
	if p.GatewayName != "" {
		body["gateway_name"] = p.GatewayName
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Action string     `json:"action"`
		Texts  *[]*string `json:"texts"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return Verdict{}, errEvaluation
	}
	switch out.Action {
	case "NONE":
		return vendorBoolean(false, "noma_v2.policy"), nil
	case "BLOCKED":
		return vendorBoolean(true, "noma_v2.policy"), nil
	case "GUARDRAIL_INTERVENED":
		if out.Texts == nil || len(*out.Texts) != 1 || (*out.Texts)[0] == nil {
			return Verdict{}, errEvaluation
		}
		return vendorReplacement(s, *(*out.Texts)[0], "noma_v2.policy")
	}
	return Verdict{}, errEvaluation
}

type zscalerParameters struct {
	PolicyID int64 `json:"policy_id,omitempty"`
}

func validateZscaler(raw json.RawMessage) error {
	var p zscalerParameters
	if decodeVendorParameters(raw, &p) != nil || p.PolicyID < 0 {
		return errEvaluation
	}
	return nil
}
func evaluateZscaler(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p zscalerParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	direction := "IN"
	if bOutput(phase) {
		direction = "OUT"
	}
	body := map[string]any{"direction": direction, "content": s.Text}
	if p.PolicyID > 0 {
		body["policyId"] = p.PolicyID
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Status int    `json:"statusCode"`
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Status != 200 {
		return Verdict{}, errEvaluation
	}
	switch out.Action {
	case "ALLOW":
		return vendorBoolean(false, "zscaler_ai_guard.policy"), nil
	case "DETECT":
		return Verdict{Action: Annotate, Categories: []string{"zscaler_ai_guard.policy"}}, nil
	case "BLOCK":
		return vendorBoolean(true, "zscaler_ai_guard.policy"), nil
	}
	return Verdict{}, errEvaluation
}

type javelinParameters struct {
	Application string          `json:"application,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
}

func validateJavelin(raw json.RawMessage) error {
	var p javelinParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.Application) {
		return errEvaluation
	}
	if len(p.Config) > 0 {
		var v map[string]json.RawMessage
		if json.Unmarshal(p.Config, &v) != nil || v == nil {
			return errEvaluation
		}
	}
	return nil
}
func evaluateJavelin(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	if bOutput(phase) {
		return Verdict{}, errEvaluation
	}
	var p javelinParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	config := p.Config
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, map[string]any{"input": map[string]string{"text": s.Text}, "metadata": map[string]string{}, "config": config}, func(req *http.Request, _ []byte) error {
		if cred.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("x-javelin-apikey", cred.APIKey)
		if p.Application != "" {
			req.Header.Set("x-javelin-application", p.Application)
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Assessments *[]map[string]struct {
			Reject *bool `json:"request_reject"`
		} `json:"assessments"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Assessments == nil {
		return Verdict{}, errEvaluation
	}
	blocked := false
	for _, assessment := range *out.Assessments {
		if len(assessment) == 0 {
			return Verdict{}, errEvaluation
		}
		for _, result := range assessment {
			if result.Reject == nil {
				return Verdict{}, errEvaluation
			}
			blocked = blocked || *result.Reject
		}
	}
	return vendorBoolean(blocked, "javelin.policy"), nil
}

type enkryptParameters struct {
	PolicyName string `json:"policy_name,omitempty"`
}

func validateEnkrypt(raw json.RawMessage) error {
	var p enkryptParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.PolicyName) {
		return errEvaluation
	}
	return nil
}
func evaluateEnkrypt(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, _ string, s Segment) (Verdict, error) {
	var p enkryptParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, map[string]string{"text": s.Text}, func(req *http.Request, _ []byte) error {
		if cred.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("apikey", cred.APIKey)
		if p.PolicyName != "" {
			req.Header.Set("x-enkrypt-policy", p.PolicyName)
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Summary map[string]json.RawMessage `json:"summary"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Summary) == 0 {
		return Verdict{}, errEvaluation
	}
	blocked := false
	for key, value := range out.Summary {
		if key == "toxicity" {
			var detections []json.RawMessage
			if json.Unmarshal(value, &detections) != nil || string(value) == "null" {
				return Verdict{}, errEvaluation
			}
			blocked = blocked || len(detections) > 0
			continue
		}
		var flag *int
		if json.Unmarshal(value, &flag) != nil || flag == nil || (*flag != 0 && *flag != 1) {
			return Verdict{}, errEvaluation
		}
		blocked = blocked || *flag == 1
	}
	return vendorBoolean(blocked, "enkryptai.policy"), nil
}

type ibmParameters struct {
	DetectorID     string          `json:"detector_id"`
	Orchestrator   bool            `json:"orchestrator,omitempty"`
	ScoreThreshold *float64        `json:"score_threshold,omitempty"`
	DetectorParams json.RawMessage `json:"detector_params,omitempty"`
}

func validateIBM(raw json.RawMessage) error {
	var p ibmParameters
	if decodeVendorParameters(raw, &p) != nil || p.DetectorID == "" || !bString(p.DetectorID) {
		return errEvaluation
	}
	if p.ScoreThreshold != nil && (math.IsNaN(*p.ScoreThreshold) || math.IsInf(*p.ScoreThreshold, 0) || *p.ScoreThreshold < 0 || *p.ScoreThreshold > 1) {
		return errEvaluation
	}
	if len(p.DetectorParams) > 0 {
		var params map[string]json.RawMessage
		if json.Unmarshal(p.DetectorParams, &params) != nil || params == nil {
			return errEvaluation
		}
	}
	return nil
}
func evaluateIBM(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, _ string, s Segment) (Verdict, error) {
	var p ibmParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	params := p.DetectorParams
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	body := map[string]any{"contents": []string{s.Text}, "detector_params": params}
	if p.Orchestrator {
		body = map[string]any{"content": s.Text, "detectors": map[string]any{p.DetectorID: params}}
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(req *http.Request, _ []byte) error {
		if cred.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
		if !p.Orchestrator {
			req.Header.Set("detector-id", p.DetectorID)
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	type detection struct {
		Score     *float64 `json:"score"`
		Type      string   `json:"detection_type"`
		Detection string   `json:"detection"`
	}
	var detections []detection
	if p.Orchestrator {
		var out struct {
			Detections *[]detection `json:"detections"`
		}
		if json.Unmarshal(raw, &out) != nil || out.Detections == nil {
			return Verdict{}, errEvaluation
		}
		detections = *out.Detections
	} else {
		var out [][]detection
		if json.Unmarshal(raw, &out) != nil || len(out) != 1 || out[0] == nil {
			return Verdict{}, errEvaluation
		}
		detections = out[0]
	}
	blocked := false
	for _, d := range detections {
		if d.Score == nil || *d.Score < 0 || *d.Score > 1 || d.Type == "" || d.Detection == "" {
			return Verdict{}, errEvaluation
		}
		if p.ScoreThreshold == nil || *d.Score >= *p.ScoreThreshold {
			blocked = true
		}
	}
	return vendorBoolean(blocked, "ibm_guardrails.policy"), nil
}

func evaluateOnyx(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	if cred.APIKey == "" {
		return Verdict{}, errEvaluation
	}
	endpoint := strings.TrimRight(c.Endpoint, "/") + "/guard/evaluate/v1/" + url.PathEscape(cred.APIKey) + "/litellm"
	payload := map[string]any{"messages": bMessages(phase, s)}
	if bOutput(phase) {
		payload = map[string]any{"object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": s.Text}, "finish_reason": "stop"}}}
	}
	// Segment IDs repeat across requests, so vendor correlation needs a fresh ID.
	raw, err := externalJSON(ctx, client, endpoint, map[string]any{"payload": payload, "input_type": bDirection(phase), "conversation_id": uuid.NewString()}, func(*http.Request, []byte) error { return nil })
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Allowed *bool `json:"allowed"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Allowed == nil {
		return Verdict{}, errEvaluation
	}
	return vendorBoolean(!*out.Allowed, "onyx.policy"), nil
}
func evaluatePromptGuard(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	direction := "input"
	if bOutput(phase) {
		direction = "output"
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, map[string]any{"messages": bMessages(phase, s), "direction": direction}, cred, "X-API-Key", "")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Decision string `json:"decision"`
		Messages *[]struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"redacted_messages"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return Verdict{}, errEvaluation
	}
	if out.Decision == "redact" {
		if out.Messages == nil || len(*out.Messages) != 1 || (*out.Messages)[0].Content == nil || (*out.Messages)[0].Role != bRole(phase, s) {
			return Verdict{}, errEvaluation
		}
		return vendorReplacement(s, *(*out.Messages)[0].Content, "promptguard.policy")
	}
	return bAction(out.Decision, "promptguard.policy")
}

type xecguardParameters struct {
	Model       string   `json:"model,omitempty"`
	PolicyNames []string `json:"policy_names,omitempty"`
}

func validateXecguard(raw json.RawMessage) error {
	var p xecguardParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.Model) || len(p.PolicyNames) > 64 {
		return errEvaluation
	}
	for _, name := range p.PolicyNames {
		if name == "" || !bString(name) {
			return errEvaluation
		}
	}
	return nil
}
func evaluateXecguard(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p xecguardParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	if p.Model == "" {
		p.Model = "xecguard_v2"
	}
	if len(p.PolicyNames) == 0 {
		p.PolicyNames = []string{"Default_Policy_SystemPromptEnforcement", "Default_Policy_HarmfulContentProtection", "Default_Policy_GeneralPromptAttackProtection"}
	}
	scanType := "input"
	if bOutput(phase) {
		scanType = "response"
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, map[string]any{"model": p.Model, "scan_type": scanType, "messages": bMessages(phase, s), "policy_names": p.PolicyNames}, cred, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return Verdict{}, errEvaluation
	}
	switch out.Decision {
	case "SAFE":
		return vendorBoolean(false, "xecguard.policy"), nil
	case "UNSAFE":
		return vendorBoolean(true, "xecguard.policy"), nil
	}
	return Verdict{}, errEvaluation
}

type promptSecurityParameters struct {
	User         string `json:"user,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

func validatePromptSecurity(raw json.RawMessage) error {
	var p promptSecurityParameters
	if decodeVendorParameters(raw, &p) != nil || !bString(p.User) || len(p.SystemPrompt) > 8192 || !utf8.ValidString(p.SystemPrompt) {
		return errEvaluation
	}
	return nil
}
func evaluatePromptSecurity(ctx context.Context, client *http.Client, c ExternalConfig, cred externalCredential, phase string, s Segment) (Verdict, error) {
	var p promptSecurityParameters
	if err := decodeVendorParameters(c.Parameters, &p); err != nil {
		return Verdict{}, err
	}
	body := map[string]any{"user": p.User, "system_prompt": p.SystemPrompt}
	if bOutput(phase) {
		body["response"] = s.Text
	} else {
		body["messages"] = bMessages(phase, s)
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, cred, "APP-ID", "")
	if err != nil {
		return Verdict{}, err
	}
	type result struct {
		Action   string  `json:"action"`
		Text     *string `json:"modified_text"`
		Messages *[]struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"modified_messages"`
	}
	var out struct {
		Result struct {
			Prompt   *result `json:"prompt"`
			Response *result `json:"response"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return Verdict{}, errEvaluation
	}
	v := out.Result.Prompt
	if bOutput(phase) {
		v = out.Result.Response
	}
	if v == nil {
		return Verdict{}, errEvaluation
	}
	if v.Action == "modify" {
		replacement := v.Text
		if !bOutput(phase) {
			if v.Messages == nil || len(*v.Messages) != 1 || (*v.Messages)[0].Content == nil || (*v.Messages)[0].Role != bRole(phase, s) {
				return Verdict{}, errEvaluation
			}
			replacement = (*v.Messages)[0].Content
		}
		if replacement == nil {
			return Verdict{}, errEvaluation
		}
		return vendorReplacement(s, *replacement, "prompt_security.policy")
	}
	return bAction(v.Action, "prompt_security.policy")
}
