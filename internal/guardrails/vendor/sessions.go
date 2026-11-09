package vendor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type hiddenlayerConfig struct {
	Connection
	AuthEndpoint string                `json:"auth_endpoint"`
	Parameters   hiddenlayerParameters `json:"parameters"`
}

type hiddenlayerParameters struct {
	ProjectID string `json:"project_id,omitempty"`
	Model     string `json:"model,omitempty"`
}

func (c hiddenlayerConfig) urls() []string {
	if c.AuthEndpoint == "" {
		return c.Connection.urls()
	}
	return []string{c.Endpoint, c.AuthEndpoint}
}

func (c hiddenlayerConfig) check() error {
	if c.AuthEndpoint != "" && !validURL(c.AuthEndpoint) {
		return errors.New("invalid guardrail authentication endpoint")
	}
	if !optional(c.Parameters.ProjectID) || !optional(c.Parameters.Model) {
		return errors.New("invalid guardrail vendor parameters")
	}
	return nil
}

func hiddenlayer(ctx context.Context, x exchange, c hiddenlayerConfig, r Request) (Decision, error) {
	token, err := hiddenlayerToken(ctx, x, c.AuthEndpoint)
	if err != nil {
		return Decision{}, err
	}
	side := pick(r, "input", "output")
	body := map[string]any{side: map[string]any{"messages": messages(r)}, "metadata": map[string]string{"model": c.Parameters.Model, "requester_id": "OLP"}}
	raw, err := x.post(ctx, body, func(req *http.Request, _ []byte) error {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("hl-runtime-edge-provider", "olp")
		req.Header.Set("hl-runtime-edge-provider-version", "1")
		if c.Parameters.ProjectID != "" {
			req.Header.Set("HL-Project-Id", c.Parameters.ProjectID)
		}
		return nil
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Evaluation *struct {
			Action string `json:"action"`
		} `json:"evaluation"`
		Modified map[string]struct {
			Messages []content `json:"messages"`
		} `json:"modified_data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Evaluation == nil {
		return Decision{}, errUnavailable
	}
	switch response.Evaluation.Action {
	case "Allow":
		return Decision{}, nil
	case "Block":
		return flag("hiddenlayer.policy"), nil
	case "Redact":
		return replaceOnly(response.Modified[side].Messages, r, "hiddenlayer.policy")
	default:
		return Decision{}, errUnavailable
	}
}

func hiddenlayerToken(ctx context.Context, x exchange, authEndpoint string) (string, error) {
	if authEndpoint == "" {
		return x.apiKey()
	}
	if x.credential.ClientID == "" || x.credential.ClientSecret == "" {
		return "", errUnavailable
	}
	u, err := url.Parse(authEndpoint)
	if err != nil {
		return "", errUnavailable
	}
	u.RawQuery = url.Values{"grant_type": {"client_credentials"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return "", errUnavailable
	}
	req.SetBasicAuth(x.credential.ClientID, x.credential.ClientSecret)
	req.Header.Set("Accept", "application/json")
	resp, err := x.client.Do(req)
	if err != nil {
		return "", errUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errUnavailable
	}
	raw, err := readJSON(resp.Body)
	if err != nil {
		return "", err
	}
	var auth struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.AccessToken == "" || len(auth.AccessToken) > 64<<10 {
		return "", errUnavailable
	}
	return auth.AccessToken, nil
}

type purviewParameters struct {
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
}

func (p purviewParameters) valid() bool {
	return required(p.ApplicationID) && required(p.ApplicationName)
}

func purview(ctx context.Context, x exchange, p purviewParameters, r Request) (Decision, error) {
	application := map[string]any{"name": p.ApplicationName, "version": "1.0", "applicationLocation": map[string]string{"@odata.type": "microsoft.graph.policyLocationApplication", "value": p.ApplicationID}}
	entry := map[string]any{"@odata.type": "microsoft.graph.processConversationMetadata", "identifier": uuid.NewString(), "content": map[string]string{"@odata.type": "microsoft.graph.textContent", "data": r.Text}, "name": p.ApplicationName + " message", "correlationId": uuid.NewString(), "sequenceNumber": 0, "isTruncated": false}
	body := map[string]any{"contentToProcess": map[string]any{
		"contentEntries":        []any{entry},
		"activityMetadata":      map[string]string{"activity": pick(r, "uploadText", "downloadText")},
		"deviceMetadata":        map[string]any{},
		"protectedAppMetadata":  application,
		"integratedAppMetadata": map[string]string{"name": p.ApplicationName, "version": "1.0"},
	}}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Actions *[]struct {
			Type        string `json:"@odata.type"`
			Action      string `json:"action"`
			Restriction string `json:"restrictionAction"`
		} `json:"policyActions"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Actions == nil {
		return Decision{}, errUnavailable
	}
	blocked := false
	for _, action := range *response.Actions {
		if action.Type == "" && action.Action == "" {
			return Decision{}, errUnavailable
		}
		if !strings.Contains(action.Type, "restrictAccessAction") && action.Action != "restrictAccess" {
			continue
		}
		switch action.Restriction {
		case "block":
			blocked = true
		case "warn", "audit":
		default:
			return Decision{}, errUnavailable
		}
	}
	return flagIf(blocked, "microsoft_purview.policy"), nil
}

type aktoParameters struct {
	AccountID string `json:"account_id"`
	VXLANID   string `json:"vxlan_id"`
}

func (p aktoParameters) valid() bool { return required(p.AccountID) && required(p.VXLANID) }

func aktoPayload(body any) (string, error) {
	inner, err := json.Marshal(body)
	if err != nil {
		return "", errUnavailable
	}
	outer, err := json.Marshal(map[string]string{"body": string(inner)})
	if err != nil {
		return "", errUnavailable
	}
	return string(outer), nil
}

func aktoPayloads(r Request) (string, string, error) {
	if !r.Output {
		request, err := aktoPayload(map[string]any{"messages": messages(r)})
		return request, "{}", err
	}
	response, err := aktoPayload(map[string]any{"choices": []any{map[string]any{"message": message(r)}}})
	return "{}", response, err
}

func akto(ctx context.Context, x exchange, p aktoParameters, r Request) (Decision, error) {
	request, response, err := aktoPayloads(r)
	if err != nil {
		return Decision{}, err
	}
	body := map[string]any{"path": "/v1/chat/completions", "requestHeaders": `{"content-type":"application/json"}`, "responseHeaders": `{"content-type":"application/json"}`, "method": "POST", "requestPayload": request, "responsePayload": response, "ip": "", "destIp": "127.0.0.1", "time": strconv.FormatInt(time.Now().UnixMilli(), 10), "statusCode": "200", "type": "HTTP/1.1", "status": "200", "akto_account_id": p.AccountID, "akto_vxlan_id": p.VXLANID, "is_pending": "false", "source": "MIRRORING", "direction": nil, "process_id": nil, "socket_id": nil, "daemonset_id": nil, "enabled_graph": nil, "tag": "{}", "metadata": "{}", "contextSource": "AGENTIC"}
	query := url.Values{"akto_connector": {"litellm"}, "guardrails": {"true"}}
	raw, err := x.at(x.endpoint+"?"+query.Encode()).keyed(ctx, body, "Authorization", "")
	if err != nil {
		return Decision{}, err
	}
	var result struct {
		Data *struct {
			GuardrailsResult *struct {
				Allowed *bool `json:"Allowed"`
			} `json:"guardrailsResult"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Data == nil || result.Data.GuardrailsResult == nil || result.Data.GuardrailsResult.Allowed == nil {
		return Decision{}, errUnavailable
	}
	return flagIf(!*result.Data.GuardrailsResult.Allowed, "akto.policy"), nil
}

type conductParameters struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
}

func (p conductParameters) valid() bool {
	return optional(p.WorkspaceID) && len(p.Model) <= 256 && len(p.Provider) <= 256
}

type conductResponse struct {
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

func conduct(ctx context.Context, x exchange, p conductParameters, r Request) (Decision, error) {
	id := uuid.NewString()
	arguments := map[string]string{"prompt": r.Text}
	if p.Model != "" {
		arguments["model"] = p.Model
	}
	if p.Provider != "" {
		arguments["provider"] = p.Provider
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "guard_check_prompt", "arguments": arguments}}
	raw, status, err := conductCall(ctx, x, body, p.WorkspaceID)
	if err != nil {
		return Decision{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return flag("conduct.identity_required"), nil
	}
	var response conductResponse
	if json.Unmarshal(raw, &response) != nil || response.JSONRPC != "2.0" || response.ID != id {
		return Decision{}, errUnavailable
	}
	if failed(response.Error) {
		var rpcError struct {
			Data struct {
				IdentityRequired bool `json:"identity_required"`
			} `json:"data"`
		}
		if json.Unmarshal(response.Error, &rpcError) == nil && rpcError.Data.IdentityRequired {
			return flag("conduct.identity_required"), nil
		}
		return Decision{}, errUnavailable
	}
	if response.Result != nil && response.Result.Structured != nil && response.Result.Structured.IdentityRequired {
		return flag("conduct.identity_required"), nil
	}
	if status < 200 || status >= 300 || response.Result == nil || response.Result.IsError {
		return Decision{}, errUnavailable
	}
	for _, part := range response.Result.Content {
		if part.Type != "text" {
			continue
		}
		decision, known, err := conductVerdict(part.Text)
		if err != nil || known {
			return decision, err
		}
	}
	return Decision{}, errUnavailable
}

func conductVerdict(raw string) (Decision, bool, error) {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "[ws:") {
		end := strings.IndexByte(text, ']')
		if end < 5 {
			return Decision{}, false, errUnavailable
		}
		text = strings.TrimSpace(text[end+1:])
	}
	switch {
	case strings.HasPrefix(text, "BLOCKED"), strings.HasPrefix(text, "PENDING approval"), strings.HasPrefix(text, "advisory"), strings.HasPrefix(text, "WARNING"):
		return flag("conduct.policy"), true, nil
	case text == "ok" || strings.HasPrefix(text, "ok "):
		return Decision{}, true, nil
	default:
		return Decision{}, false, nil
	}
}

func conductCall(ctx context.Context, x exchange, body any, workspace string) ([]byte, int, error) {
	key, err := x.apiKey()
	if err != nil {
		return nil, 0, err
	}
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > maxRequestBytes {
		return nil, 0, errUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, errUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Claude-Surface", "olp")
	if workspace != "" {
		req.Header.Set("X-Workspace-Id", workspace)
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return nil, 0, errUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, resp.StatusCode, nil
	}
	raw, err := readJSON(resp.Body)
	return raw, resp.StatusCode, err
}
