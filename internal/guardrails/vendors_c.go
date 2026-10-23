package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// These adapters use the exact operation URL in endpoint. Only documented
// operation selectors (Akto query flags) are added by the adapter.
var namedVendorsC = func() map[string]namedVendor {
	result := make(map[string]namedVendor)
	for _, kind := range []string{"qualifire", "ovalix", "microsoft_purview", "akto", "deepkeep", "qostodian_nexus", "rubrik", "vigil_guard", "repelloai", "singulr", "headroom", "compresr", "straiker", "alice", "conduct"} {
		result[kind] = namedVendor{
			validate: func(raw json.RawMessage) error { return validateVendorC(kind, raw) },
			evaluate: func(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, segment Segment) (Verdict, error) {
				return evaluateVendorC(ctx, client, c, credential, kind, phase, segment)
			},
		}
	}
	for _, kind := range []string{"ovalix", "deepkeep", "qostodian_nexus", "vigil_guard", "headroom", "compresr", "straiker", "alice"} {
		adapter := result[kind]
		adapter.transforms = true
		result[kind] = adapter
	}
	for _, kind := range []string{"headroom", "compresr"} {
		adapter := result[kind]
		adapter.phases = []string{Input}
		result[kind] = adapter
	}
	conduct := result["conduct"]
	conduct.phases = []string{Input, InputParallel}
	result["conduct"] = conduct
	return result
}()

type qualifireParameters struct {
	EvaluationID           string   `json:"evaluation_id,omitempty"`
	Assertions             []string `json:"assertions,omitempty"`
	PromptInjections       bool     `json:"prompt_injections,omitempty"`
	HallucinationsCheck    bool     `json:"hallucinations_check,omitempty"`
	GroundingCheck         bool     `json:"grounding_check,omitempty"`
	PIICheck               bool     `json:"pii_check,omitempty"`
	ContentModerationCheck bool     `json:"content_moderation_check,omitempty"`
}

type ovalixParameters struct {
	ApplicationID string `json:"application_id"`
	CheckpointID  string `json:"checkpoint_id"`
	Actor         string `json:"actor"`
}

type purviewParameters struct {
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
}

type aktoParameters struct {
	AccountID string `json:"account_id"`
	VXLANID   string `json:"vxlan_id"`
}

type deepkeepParameters struct {
	FirewallID string `json:"firewall_id"`
}

type qostodianParameters struct {
	Source    string `json:"source,omitempty"`
	Container string `json:"container,omitempty"`
	Identity  string `json:"identity,omitempty"`
}

type repelloParameters struct {
	AssetID string `json:"asset_id"`
}

type singulrParameters struct {
	ApplicationID string `json:"application_id,omitempty"`
	GuardrailID   string `json:"guardrail_id,omitempty"`
}

type headroomParameters struct {
	Model string `json:"model,omitempty"`
}

type compresrParameters struct {
	Query                  string  `json:"query"`
	Model                  string  `json:"compression_model_name,omitempty"`
	TargetCompressionRatio float64 `json:"target_compression_ratio,omitempty"`
	Coarse                 bool    `json:"coarse,omitempty"`
	Dynamic                bool    `json:"dynamic,omitempty"`
}

type straikerParameters struct {
	Source string `json:"source,omitempty"`
}

type conductParameters struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
}

func validateVendorC(kind string, raw json.RawMessage) error {
	var target any
	switch kind {
	case "qualifire":
		p := &qualifireParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if p.EvaluationID == "" && len(p.Assertions) == 0 && !p.PromptInjections && !p.HallucinationsCheck && !p.GroundingCheck && !p.PIICheck && !p.ContentModerationCheck {
			return errEvaluation
		}
		if len(p.Assertions) > 64 || len(p.EvaluationID) > 256 {
			return errEvaluation
		}
		for _, assertion := range p.Assertions {
			if len(assertion) == 0 || len(assertion) > 8192 {
				return errEvaluation
			}
		}
		return nil
	case "ovalix":
		p := &ovalixParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if !vendorCID(p.ApplicationID) || !vendorCID(p.CheckpointID) || !vendorCID(p.Actor) {
			return errEvaluation
		}
		return nil
	case "microsoft_purview":
		p := &purviewParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if !vendorCID(p.ApplicationID) || !vendorCID(p.ApplicationName) {
			return errEvaluation
		}
		return nil
	case "akto":
		p := &aktoParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if !vendorCID(p.AccountID) || !vendorCID(p.VXLANID) {
			return errEvaluation
		}
		return nil
	case "deepkeep":
		p := &deepkeepParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if !vendorCID(p.FirewallID) {
			return errEvaluation
		}
		return nil
	case "repelloai":
		p := &repelloParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if !vendorCID(p.AssetID) {
			return errEvaluation
		}
		return nil
	case "singulr":
		p := &singulrParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if p.ApplicationID != "" && !vendorCID(p.ApplicationID) || p.GuardrailID != "" && !vendorCID(p.GuardrailID) {
			return errEvaluation
		}
		return nil
	case "qostodian_nexus":
		p := &qostodianParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		for _, v := range []string{p.Source, p.Container, p.Identity} {
			if v != "" && !vendorCID(v) {
				return errEvaluation
			}
		}
		return nil
	case "headroom":
		target = &headroomParameters{}
	case "compresr":
		p := &compresrParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if p.Query == "" || len(p.Query) > 8192 || len(p.Model) > 256 || p.TargetCompressionRatio < 0 || p.TargetCompressionRatio > 1 {
			return errEvaluation
		}
		return nil
	case "straiker":
		target = &straikerParameters{}
	case "conduct":
		p := &conductParameters{}
		if err := decodeVendorParameters(raw, p); err != nil {
			return err
		}
		if p.WorkspaceID != "" && !vendorCID(p.WorkspaceID) || len(p.Model) > 256 || len(p.Provider) > 256 {
			return errEvaluation
		}
		return nil
	default:
		target = &struct{}{}
	}
	return decodeVendorParameters(raw, target)
}

func vendorCID(value string) bool {
	return len(value) > 0 && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

func vendorCInputType(phase string) string {
	if phase == Output || phase == OutputStream {
		return "response"
	}
	return "request"
}

func vendorCMessage(segment Segment, phase string) map[string]string {
	role := segment.Role
	if role == "" {
		role = "user"
		if vendorCInputType(phase) == "response" {
			role = "assistant"
		}
	}
	return map[string]string{"role": role, "content": segment.Text}
}

func evaluateVendorC(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, kind, phase string, s Segment) (Verdict, error) {
	if err := validateVendorC(kind, c.Parameters); err != nil {
		return Verdict{}, err
	}
	switch kind {
	case "qualifire":
		return evaluateQualifire(ctx, client, c, credential, phase, s)
	case "ovalix":
		return evaluateOvalix(ctx, client, c, credential, phase, s)
	case "microsoft_purview":
		return evaluatePurview(ctx, client, c, credential, phase, s)
	case "akto":
		return evaluateAkto(ctx, client, c, credential, phase, s)
	case "deepkeep", "qostodian_nexus":
		return evaluateTextAPI(ctx, client, c, credential, kind, phase, s)
	case "rubrik":
		return evaluateRubrik(ctx, client, c, credential, phase, s)
	case "vigil_guard":
		return evaluateVigil(ctx, client, c, credential, phase, s)
	case "repelloai":
		return evaluateRepello(ctx, client, c, credential, phase, s)
	case "singulr":
		return evaluateSingulr(ctx, client, c, credential, phase, s)
	case "headroom", "compresr":
		return evaluateCompression(ctx, client, c, credential, kind, s)
	case "straiker":
		return evaluateStraiker(ctx, client, c, credential, phase, s)
	case "alice":
		return evaluateAlice(ctx, client, c, credential, phase, s)
	case "conduct":
		return evaluateConduct(ctx, client, c, credential, s)
	}
	return Verdict{}, errEvaluation
}

func evaluateQualifire(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p qualifireParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	encoded, _ := json.Marshal(p)
	var body map[string]any
	_ = json.Unmarshal(encoded, &body)
	body["messages"] = []any{vendorCMessage(s, phase)}
	if p.EvaluationID != "" {
		body["input"], body["output"] = s.Text, ""
	}
	if vendorCInputType(phase) == "response" {
		body["output"] = s.Text
		body["messages"] = []any{}
		if p.EvaluationID != "" {
			body["input"] = ""
		}
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "X-Qualifire-API-Key", "")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		EvaluationResults *[]struct {
			Results *[]struct {
				Flagged *bool `json:"flagged"`
			} `json:"results"`
		} `json:"evaluationResults"`
	}
	if json.Unmarshal(raw, &response) != nil || response.EvaluationResults == nil || len(*response.EvaluationResults) == 0 {
		return Verdict{}, errEvaluation
	}
	blocked := false
	for _, evaluation := range *response.EvaluationResults {
		if evaluation.Results == nil || len(*evaluation.Results) == 0 {
			return Verdict{}, errEvaluation
		}
		for _, result := range *evaluation.Results {
			if result.Flagged == nil {
				return Verdict{}, errEvaluation
			}
			blocked = blocked || *result.Flagged
		}
	}
	return vendorBoolean(blocked, "qualifire.policy"), nil
}

func evaluateOvalix(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p ovalixParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	body := map[string]any{"application_id": p.ApplicationID, "checkpoint_id": p.CheckpointID, "actor": p.Actor, "session_id": uuid.NewString(), "data_type": "TEXT", "data": map[string]string{"content": s.Text}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Action   string `json:"action_type"`
		Modified *struct {
			Content *string `json:"content"`
		} `json:"modified_data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Action == "" {
		return Verdict{}, errEvaluation
	}
	switch response.Action {
	case "block":
		return vendorBoolean(true, "ovalix.policy"), nil
	case "allow", "anonymize":
	default:
		return Verdict{}, errEvaluation
	}
	if response.Modified != nil && response.Modified.Content != nil && *response.Modified.Content != s.Text {
		return vendorReplacement(s, *response.Modified.Content, "ovalix.policy")
	}
	if response.Action == "anonymize" && (response.Modified == nil || response.Modified.Content == nil) {
		return Verdict{}, errEvaluation
	}
	return Verdict{Action: "pass"}, nil
}

func evaluatePurview(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p purviewParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	activity := "uploadText"
	if vendorCInputType(phase) == "response" {
		activity = "downloadText"
	}
	application := map[string]any{"name": p.ApplicationName, "version": "1.0", "applicationLocation": map[string]string{"@odata.type": "microsoft.graph.policyLocationApplication", "value": p.ApplicationID}}
	body := map[string]any{"contentToProcess": map[string]any{
		"contentEntries":   []any{map[string]any{"@odata.type": "microsoft.graph.processConversationMetadata", "identifier": uuid.NewString(), "content": map[string]string{"@odata.type": "microsoft.graph.textContent", "data": s.Text}, "name": p.ApplicationName + " message", "correlationId": uuid.NewString(), "sequenceNumber": 0, "isTruncated": false}},
		"activityMetadata": map[string]string{"activity": activity}, "deviceMetadata": map[string]any{}, "protectedAppMetadata": application,
		"integratedAppMetadata": map[string]string{"name": p.ApplicationName, "version": "1.0"},
	}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Actions *[]struct {
			Type        string `json:"@odata.type"`
			Action      string `json:"action"`
			Restriction string `json:"restrictionAction"`
		} `json:"policyActions"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Actions == nil {
		return Verdict{}, errEvaluation
	}
	blocked := false
	for _, action := range *response.Actions {
		if strings.Contains(action.Type, "restrictAccessAction") || action.Action == "restrictAccess" {
			switch action.Restriction {
			case "block":
				blocked = true
			case "warn", "audit":
			default:
				return Verdict{}, errEvaluation
			}
		}
		if action.Type == "" && action.Action == "" {
			return Verdict{}, errEvaluation
		}
	}
	return vendorBoolean(blocked, "microsoft_purview.policy"), nil
}

func evaluateAkto(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p aktoParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	requestBody, _ := json.Marshal(map[string]any{"messages": []any{vendorCMessage(s, phase)}})
	requestWrapper, _ := json.Marshal(map[string]string{"body": string(requestBody)})
	responseWrapper := "{}"
	if vendorCInputType(phase) == "response" {
		responseBody, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": vendorCMessage(s, phase)}}})
		encoded, _ := json.Marshal(map[string]string{"body": string(responseBody)})
		responseWrapper = string(encoded)
		requestWrapper = []byte(`{}`)
	}
	body := map[string]any{"path": "/v1/chat/completions", "requestHeaders": `{"content-type":"application/json"}`, "responseHeaders": `{"content-type":"application/json"}`, "method": "POST", "requestPayload": string(requestWrapper), "responsePayload": responseWrapper, "ip": "", "destIp": "127.0.0.1", "time": strconv.FormatInt(time.Now().UnixMilli(), 10), "statusCode": "200", "type": "HTTP/1.1", "status": "200", "akto_account_id": p.AccountID, "akto_vxlan_id": p.VXLANID, "is_pending": "false", "source": "MIRRORING", "direction": nil, "process_id": nil, "socket_id": nil, "daemonset_id": nil, "enabled_graph": nil, "tag": "{}", "metadata": "{}", "contextSource": "AGENTIC"}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return Verdict{}, errEvaluation
	}
	query := u.Query()
	query.Set("akto_connector", "litellm")
	query.Set("guardrails", "true")
	u.RawQuery = query.Encode()
	raw, err := vendorJSON(ctx, client, u.String(), body, credential, "Authorization", "")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Data *struct {
			GuardrailsResult *struct {
				Allowed *bool `json:"Allowed"`
			} `json:"guardrailsResult"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Data == nil || response.Data.GuardrailsResult == nil || response.Data.GuardrailsResult.Allowed == nil {
		return Verdict{}, errEvaluation
	}
	return vendorBoolean(!*response.Data.GuardrailsResult.Allowed, "akto.policy"), nil
}

func evaluateTextAPI(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, kind, phase string, s Segment) (Verdict, error) {
	body := map[string]any{"texts": []string{s.Text}, "input_type": vendorCInputType(phase), "request_data": map[string]any{}, "litellm_call_id": nil, "litellm_trace_id": nil, "litellm_version": "1.105.0", "images": nil, "tools": nil, "structured_messages": nil, "tool_calls": nil, "model": nil, "additional_provider_specific_params": map[string]any{}}
	if kind == "deepkeep" {
		var p deepkeepParameters
		_ = decodeVendorParameters(c.Parameters, &p)
		body["additional_provider_specific_params"] = map[string]string{"firewall_id": p.FirewallID}
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(request *http.Request, _ []byte) error {
		if credential.APIKey == "" {
			return errEvaluation
		}
		request.Header.Set("X-API-Key", credential.APIKey)
		if kind == "qostodian_nexus" {
			var p qostodianParameters
			_ = decodeVendorParameters(c.Parameters, &p)
			request.Header.Set("x-qostodian-nexus-identifiers-trace", uuid.NewString())
			for name, value := range map[string]string{"source": p.Source, "container": p.Container, "identity": p.Identity} {
				if value != "" {
					request.Header.Set("x-qostodian-nexus-identifiers-"+name, value)
				}
			}
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Action string     `json:"action"`
		Texts  *[]*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Action {
	case "BLOCKED":
		return vendorBoolean(true, kind+".policy"), nil
	case "NONE", "GUARDRAIL_INTERVENED":
		if response.Texts != nil {
			if len(*response.Texts) != 1 || (*response.Texts)[0] == nil {
				return Verdict{}, errEvaluation
			}
			if *(*response.Texts)[0] != s.Text {
				return vendorReplacement(s, *(*response.Texts)[0], kind+".policy")
			}
		}
		if response.Action == "GUARDRAIL_INTERVENED" && response.Texts == nil {
			return Verdict{}, errEvaluation
		}
		return vendorBoolean(response.Action == "GUARDRAIL_INTERVENED", kind+".policy"), nil
	default:
		return Verdict{}, errEvaluation
	}
}

func evaluateRubrik(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	body := map[string]any{"model": "", "messages": []any{vendorCMessage(s, phase)}}
	output := vendorCInputType(phase) == "response"
	if output {
		body = map[string]any{"id": uuid.NewString(), "object": "chat.completion", "created": time.Now().Unix(), "model": "", "choices": []any{map[string]any{"index": 0, "message": vendorCMessage(s, phase), "finish_reason": "stop"}}}
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Choices *[]struct {
			Message *struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	if !output && response.Choices == nil {
		// The before_prompt operation deliberately returns an object with no
		// choices for an allowed request, including an empty object.
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return Verdict{}, errEvaluation
		}
		return Verdict{Action: "pass"}, nil
	}
	if response.Choices == nil || len(*response.Choices) != 1 || (*response.Choices)[0].Message == nil || (*response.Choices)[0].Message.Content == nil {
		return Verdict{}, errEvaluation
	}
	if !output {
		return vendorBoolean(true, "rubrik.policy"), nil
	}
	return vendorBoolean(*(*response.Choices)[0].Message.Content != s.Text, "rubrik.policy"), nil
}

func evaluateVigil(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	source := "user_input"
	if vendorCInputType(phase) == "response" {
		source = "model_output"
	}
	body := map[string]any{"text": s.Text, "source": source, "mode": "full", "metadata": map[string]any{}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "Authorization", "Bearer ")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Decision  string  `json:"decision"`
		Sanitized *string `json:"sanitizedText"`
		Output    *string `json:"outputText"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Decision {
	case "ALLOWED":
		return Verdict{Action: "pass"}, nil
	case "BLOCKED":
		return vendorBoolean(true, "vigil_guard.policy"), nil
	case "SANITIZED":
		if response.Sanitized != nil {
			return vendorReplacement(s, *response.Sanitized, "vigil_guard.policy")
		}
		if response.Output != nil {
			return vendorReplacement(s, *response.Output, "vigil_guard.policy")
		}
	}
	return Verdict{}, errEvaluation
}

func evaluateRepello(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p repelloParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	stage := "prompt"
	if vendorCInputType(phase) == "response" {
		stage = "response"
	}
	body := map[string]any{"asset_id": p.AssetID, "scan_data": map[string]string{stage: s.Text}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "X-API-Key", "")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Verdict {
	case "blocked":
		return vendorBoolean(true, "repelloai.policy"), nil
	case "flagged":
		return Verdict{Action: Annotate, Categories: []string{"repelloai.policy"}}, nil
	case "passed":
		return Verdict{Action: "pass"}, nil
	}
	return Verdict{}, errEvaluation
}

func evaluateSingulr(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p singulrParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	body := map[string]any{"guardrail_scope": vendorCInputType(phase)}
	if vendorCInputType(phase) == "response" {
		body["response"] = vendorCMessage(s, phase)
	} else {
		body["messages"] = []any{vendorCMessage(s, phase)}
	}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(request *http.Request, _ []byte) error {
		if credential.APIKey == "" {
			return errEvaluation
		}
		request.Header.Set("X-Singulr-Gateway-Token", credential.APIKey)
		if p.ApplicationID != "" {
			request.Header.Set("X-Singulr-Enforcement-Entity-Id", p.ApplicationID)
		}
		if p.GuardrailID != "" {
			request.Header.Set("X-Singulr-Guardrail-Id", p.GuardrailID)
		}
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Block *bool `json:"should_block"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Block == nil {
		return Verdict{}, errEvaluation
	}
	return vendorBoolean(*response.Block, "singulr.policy"), nil
}

func evaluateCompression(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, kind string, s Segment) (Verdict, error) {
	var body any
	header, prefix := "Authorization", "Bearer "
	if kind == "headroom" {
		var p headroomParameters
		_ = decodeVendorParameters(c.Parameters, &p)
		request := map[string]any{"messages": []any{vendorCMessage(s, Input)}}
		if p.Model != "" {
			request["model"] = p.Model
		}
		body = request
	} else {
		var p compresrParameters
		_ = decodeVendorParameters(c.Parameters, &p)
		if p.Model == "" {
			p.Model = "latte_v2"
		}
		if p.TargetCompressionRatio == 0 {
			p.TargetCompressionRatio = 0.5
		}
		body = map[string]any{"context": s.Text, "query": p.Query, "compression_model_name": p.Model, "target_compression_ratio": p.TargetCompressionRatio, "coarse": p.Coarse, "dynamic": p.Dynamic, "source": "integration:olp"}
		header, prefix = "X-API-Key", ""
	}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, header, prefix)
	if err != nil {
		return Verdict{}, err
	}
	var replacement *string
	if kind == "headroom" {
		var response struct {
			Messages []struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(raw, &response) != nil || len(response.Messages) != 1 || response.Messages[0].Role != vendorCMessage(s, Input)["role"] {
			return Verdict{}, errEvaluation
		}
		replacement = response.Messages[0].Content
	} else {
		var response struct {
			Data *struct {
				Context *string `json:"compressed_context"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Data == nil {
			return Verdict{}, errEvaluation
		}
		replacement = response.Data.Context
	}
	if replacement == nil {
		return Verdict{}, errEvaluation
	}
	if *replacement == s.Text {
		return Verdict{Action: "pass"}, nil
	}
	return vendorReplacement(s, *replacement, kind+".compression")
}

func evaluateStraiker(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	var p straikerParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	if p.Source == "" {
		p.Source = "olp"
	}
	event, contentKey := "pre_call", "request"
	if vendorCInputType(phase) == "response" {
		event, contentKey = "post_call", "response"
	}
	body := map[string]any{"schema_version": "1", "event": map[string]string{"type": event, "id": uuid.NewString()}, "request": map[string]any{"texts": []string{}}, "context": map[string]string{"call_surface": "completion"}, "identity": map[string]any{}, "application": map[string]string{"source": p.Source}}
	body[contentKey] = map[string]any{"texts": []string{s.Text}, "images": []string{}}
	raw, err := externalJSON(ctx, client, c.Endpoint, body, func(request *http.Request, _ []byte) error {
		if credential.APIKey == "" {
			return errEvaluation
		}
		request.Header.Set("Authorization", "Bearer "+credential.APIKey)
		request.Header.Set("X-Straiker-Webhook-Format", "litellm")
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Action string     `json:"action"`
		Texts  *[]*string `json:"texts"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Action {
	case "BLOCKED":
		return vendorBoolean(true, "straiker.policy"), nil
	case "NONE":
		return Verdict{Action: "pass"}, nil
	case "GUARDRAIL_INTERVENED":
		if response.Texts == nil || len(*response.Texts) != 1 || (*response.Texts)[0] == nil {
			return Verdict{}, errEvaluation
		}
		return vendorReplacement(s, *(*response.Texts)[0], "straiker.policy")
	}
	return Verdict{}, errEvaluation
}

func evaluateAlice(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, phase string, s Segment) (Verdict, error) {
	body := map[string]any{"input_type": vendorCInputType(phase), "inputs": map[string]any{"texts": []string{s.Text}}, "request_data": map[string]any{}}
	raw, err := vendorJSON(ctx, client, c.Endpoint, body, credential, "af-api-key", "")
	if err != nil {
		return Verdict{}, err
	}
	var response struct {
		Verdict      string `json:"verdict"`
		Replacements []struct {
			Index *int    `json:"index"`
			Text  *string `json:"text"`
		} `json:"replacements"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Verdict{}, errEvaluation
	}
	switch response.Verdict {
	case "ALLOW":
		return Verdict{Action: "pass"}, nil
	case "DETECT":
		return Verdict{Action: Annotate, Categories: []string{"alice.policy"}}, nil
	case "BLOCK":
		return vendorBoolean(true, "alice.policy"), nil
	case "MASK":
		if len(response.Replacements) != 1 || response.Replacements[0].Index == nil || *response.Replacements[0].Index != 0 || response.Replacements[0].Text == nil {
			return Verdict{}, errEvaluation
		}
		return vendorReplacement(s, *response.Replacements[0].Text, "alice.policy")
	}
	return Verdict{}, errEvaluation
}

func evaluateConduct(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, s Segment) (Verdict, error) {
	var p conductParameters
	_ = decodeVendorParameters(c.Parameters, &p)
	id := uuid.NewString()
	arguments := map[string]string{"prompt": s.Text}
	if p.Model != "" {
		arguments["model"] = p.Model
	}
	if p.Provider != "" {
		arguments["provider"] = p.Provider
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "guard_check_prompt", "arguments": arguments}}
	raw, status, err := conductJSON(ctx, client, c.Endpoint, body, credential, p.WorkspaceID)
	if err != nil {
		return Verdict{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return vendorBoolean(true, "conduct.identity_required"), nil
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured *struct {
				IdentityRequired bool `json:"identity_required"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || response.JSONRPC != "2.0" || response.ID != id {
		return Verdict{}, errEvaluation
	}
	var rpcError struct {
		Data struct {
			IdentityRequired bool `json:"identity_required"`
		} `json:"data"`
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		if json.Unmarshal(response.Error, &rpcError) == nil && rpcError.Data.IdentityRequired {
			return vendorBoolean(true, "conduct.identity_required"), nil
		}
		return Verdict{}, errEvaluation
	}
	if response.Result != nil && response.Result.Structured != nil && response.Result.Structured.IdentityRequired {
		return vendorBoolean(true, "conduct.identity_required"), nil
	}
	if status < 200 || status >= 300 {
		return Verdict{}, errEvaluation
	}
	if response.Result == nil || response.Result.IsError {
		return Verdict{}, errEvaluation
	}
	for _, part := range response.Result.Content {
		if part.Type != "text" {
			continue
		}
		text := strings.TrimSpace(part.Text)
		if strings.HasPrefix(text, "[ws:") {
			end := strings.IndexByte(text, ']')
			if end < 5 {
				return Verdict{}, errEvaluation
			}
			text = strings.TrimSpace(text[end+1:])
		}
		switch {
		case strings.HasPrefix(text, "BLOCKED"), strings.HasPrefix(text, "PENDING approval"):
			return vendorBoolean(true, "conduct.policy"), nil
		case text == "ok" || strings.HasPrefix(text, "ok "):
			return Verdict{Action: "pass"}, nil
		case strings.HasPrefix(text, "advisory"), strings.HasPrefix(text, "WARNING"):
			return Verdict{Action: Annotate, Categories: []string{"conduct.policy"}}, nil
		}
	}
	return Verdict{}, errEvaluation
}

// Conduct identity denials are policy blocks even when availability failures
// use fail-open. Retain the status until this distinction is made.
func conductJSON(ctx context.Context, client *http.Client, endpoint string, body any, credential externalCredential, workspace string) ([]byte, int, error) {
	if credential.APIKey == "" || strings.ContainsAny(credential.APIKey, "\r\n\x00") {
		return nil, 0, errEvaluation
	}
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > MaxContentBytes {
		return nil, 0, errEvaluation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, errEvaluation
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential.APIKey)
	request.Header.Set("X-Claude-Surface", "olp")
	if workspace != "" {
		request.Header.Set("X-Workspace-Id", workspace)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, errEvaluation
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, response.StatusCode, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || !json.Valid(raw) {
		return nil, 0, errEvaluation
	}
	return raw, response.StatusCode, nil
}
