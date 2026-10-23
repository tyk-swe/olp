package guardrails

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/tyk-swe/olp/internal/egress"
)

type ExternalConfig struct {
	Endpoint           string                    `json:"endpoint,omitempty"`
	AuthEndpoint       string                    `json:"auth_endpoint,omitempty"`
	AnonymizerEndpoint string                    `json:"anonymizer_endpoint,omitempty"`
	CredentialRef      string                    `json:"credential_ref,omitempty"`
	Network            *egress.ConnectionOptions `json:"network,omitempty"`
	ContentEgress      bool                      `json:"content_egress,omitempty"`
	Region             string                    `json:"region,omitempty"`
	Guardrail          string                    `json:"guardrail,omitempty"`
	Version            string                    `json:"version,omitempty"`
	Template           string                    `json:"template,omitempty"`
	Language           string                    `json:"language,omitempty"`
	Threshold          int                       `json:"threshold,omitempty"`
	Route              string                    `json:"route,omitempty"`
	Rule               *ModelRule                `json:"rule,omitempty"`
	Semantic           *SemanticRule             `json:"semantic,omitempty"`
	PluginID           string                    `json:"plugin_id,omitempty"`
	PluginVersion      string                    `json:"plugin_version,omitempty"`
	Parameters         json.RawMessage           `json:"parameters,omitempty"`
}
type ModelRule struct {
	Pointer  string          `json:"pointer"`
	Equals   json.RawMessage `json:"equals"`
	Category string          `json:"category"`
	Prompt   string          `json:"prompt,omitempty"`
}

// ValidateCredential accepts the sealed JSON credentials supported by the
// guardrail evaluator and its network connection.
func ValidateCredential(secret []byte) error {
	var c externalCredential
	if len(secret) > 0 && len(secret) <= 256<<10 && decodeConfig(secret, &c) == nil && (c.APIKey != "" || len(c.SigningKey) >= 32 || (c.AccessKeyID != "" && c.SecretAccessKey != "") || (c.ClientID != "" && c.ClientSecret != "")) {
		return nil
	}
	if len(secret) > 0 && len(secret) <= 256<<10 && egress.ValidateConnectionSecret(secret) == nil {
		return nil
	}
	return errors.New("supply a bounded guardrail credential object")
}

func ValidateExternal(d Revision) error {
	var c ExternalConfig
	if err := decodeConfig(d.Configuration, &c); err != nil {
		return err
	}
	if d.Type == "plugin" {
		if !identifier.MatchString(c.PluginID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(c.PluginVersion) {
			return errors.New("pin an approved plugin version")
		}
		return nil
	}
	if d.Type == "semantic_guard" {
		if !identifier.MatchString(c.Route) || c.Rule != nil {
			return errors.New("semantic guardrails require an embedding route and semantic rule")
		}
		return ValidateSemantic(c.Semantic)
	}
	if d.Type == "model" || d.Type == "decision_model" {
		if !identifier.MatchString(c.Route) || c.Rule == nil || !categoryID.MatchString(c.Rule.Category) || !json.Valid(c.Rule.Equals) || len(c.Rule.Pointer) > 512 || len(c.Rule.Prompt) > 8192 {
			return errors.New("invalid model guardrail route or verdict rule")
		}
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if !c.ContentEgress || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(c.Endpoint) > 2048 {
		return errors.New("external guardrails require an endpoint and explicit content egress acknowledgement")
	}
	if len(c.CredentialRef) < 1 || len(c.CredentialRef) > 100 || strings.ContainsAny(c.CredentialRef, "\r\n\x00") {
		return errors.New("external guardrails require a credential reference")
	}
	if c.AuthEndpoint != "" {
		u, e := url.Parse(c.AuthEndpoint)
		if d.Type != "hiddenlayer" || e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(c.AuthEndpoint) > 2048 {
			return errors.New("invalid guardrail authentication endpoint")
		}
	}
	if c.AnonymizerEndpoint != "" {
		u, e := url.Parse(c.AnonymizerEndpoint)
		if d.Type != "presidio" || e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(c.AnonymizerEndpoint) > 2048 {
			return errors.New("invalid Presidio anonymizer endpoint")
		}
	}
	if c.Threshold < 0 || c.Threshold > 7 {
		return errors.New("invalid content safety threshold")
	}
	if d.Type == "bedrock" && (c.Region == "" || !identifier.MatchString(c.Guardrail) || !identifier.MatchString(c.Version)) {
		return errors.New("Bedrock requires region, guardrail and version")
	}
	if d.Type == "model_armor" && (!strings.HasPrefix(c.Template, "projects/") || strings.Contains(c.Template, "..")) {
		return errors.New("Model Armor requires a template resource")
	}
	if vendor, ok := lookupNamedVendor(d.Type); ok {
		if vendor.validate != nil {
			return vendor.validate(c.Parameters)
		}
		return decodeVendorParameters(c.Parameters, &struct{}{})
	}
	if len(c.Parameters) != 0 {
		return errors.New("this guardrail does not accept vendor parameters")
	}
	return nil
}

type externalDetector struct {
	definition Revision
	evaluator  Evaluator
}

func (externalDetector) width() int { return 0 }
func (d externalDetector) detect(ctx context.Context, e Evaluation) (Verdict, error) {
	if d.evaluator == nil {
		return Verdict{}, errEvaluation
	}
	return d.evaluator.Evaluate(ctx, d.definition, e)
}

// ExternalRunner shares transport pools, but breakers belong to one immutable
// guardrail revision. Credentials come from the pinned release authority.
type ExternalRunner struct {
	Egress             egress.Policy
	Clients            *egress.ConnectionClientCache
	Credentials        func(context.Context, string) ([]byte, error)
	NetworkCredentials func(context.Context, string) ([]byte, error)
	Model              Evaluator
	Plugin             Evaluator
	mu                 sync.Mutex
	breakers           map[string]*circuit
}
type circuit struct {
	failures int
	until    time.Time
	probe    bool
}
type externalCredential struct {
	APIKey          string `json:"api_key"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	SigningKey      string `json:"signing_key"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

func (r *ExternalRunner) enter(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breakers == nil {
		r.breakers = map[string]*circuit{}
	}
	c := r.breakers[id]
	if c == nil {
		if len(r.breakers) >= 10000 {
			return false
		}
		r.breakers[id] = &circuit{}
		return true
	}
	if c.failures < 5 {
		return true
	}
	if time.Now().Before(c.until) || c.probe {
		return false
	}
	c.probe = true
	return true
}
func (r *ExternalRunner) leave(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.breakers[id]
	c.probe = false
	if err == nil {
		c.failures = 0
		c.until = time.Time{}
	} else {
		c.failures++
		if c.failures >= 5 {
			c.until = time.Now().Add(30 * time.Second)
		}
	}
}
func (r *ExternalRunner) Evaluate(ctx context.Context, d Revision, e Evaluation) (verdict Verdict, err error) {
	// One deadline covers credential resolution and every segment/vendor call.
	if d.TimeoutMS < 1 || d.TimeoutMS > 300000 || validateSegments(e.Segments) != nil || !validPhase(e.Phase) {
		return verdict, errEvaluation
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.TimeoutMS)*time.Millisecond)
	defer cancel()
	if !r.enter(d.ID) {
		return verdict, errEvaluation
	}
	defer func() {
		if err == nil {
			err = validateVerdict(verdict, e.Segments)
		}
		r.leave(d.ID, err)
		if err != nil {
			err = errEvaluation
		}
	}()
	var c ExternalConfig
	if err = decodeConfig(d.Configuration, &c); err != nil {
		return
	}
	if vendor, ok := lookupNamedVendor(d.Type); ok {
		if err = ValidateExternal(d); err != nil {
			return
		}
		if len(vendor.phases) > 0 && !slices.Contains(vendor.phases, e.Phase) {
			return verdict, errEvaluation
		}
	}
	if isModelGuardrail(d.Type) {
		if r.Model == nil {
			return verdict, errEvaluation
		}
		return r.Model.Evaluate(ctx, d, e)
	}
	if d.Type == "plugin" {
		if r.Plugin == nil {
			return verdict, errEvaluation
		}
		return r.Plugin.Evaluate(ctx, d, e)
	}
	if _, err = r.Egress.ValidateEndpoint(c.Endpoint); err != nil {
		return
	}
	if c.AuthEndpoint != "" {
		if _, err = r.Egress.ValidateEndpoint(c.AuthEndpoint); err != nil {
			return
		}
	}
	if c.AnonymizerEndpoint != "" {
		if _, err = r.Egress.ValidateEndpoint(c.AnonymizerEndpoint); err != nil {
			return
		}
	}
	if err = r.Egress.ValidateConnection(c.Network); err != nil {
		return
	}
	if r.Credentials == nil || r.Clients == nil {
		return verdict, errEvaluation
	}
	var secret []byte
	secret, err = r.Credentials(ctx, c.CredentialRef)
	if err != nil {
		return
	}
	var credential externalCredential
	if err = json.Unmarshal(secret, &credential); err != nil {
		return
	}
	var network []byte
	if c.Network != nil && c.Network.CredentialID != "" {
		if r.NetworkCredentials == nil {
			return verdict, errEvaluation
		}
		network, err = r.NetworkCredentials(ctx, c.Network.CredentialID)
		if err != nil {
			return
		}
	}
	var client *http.Client
	client, err = r.Clients.ClientScoped("guardrail/"+d.GuardrailID+"/"+d.ID, r.Egress, c.Network, network, time.Duration(d.TimeoutMS)*time.Millisecond)
	if err != nil {
		return
	}
	if d.Type == "webhook" {
		if len(credential.SigningKey) < 32 {
			return verdict, errEvaluation
		}
		var raw []byte
		raw, err = externalJSON(ctx, client, c.Endpoint, map[string]any{"guardrail": d.Name, "phase": e.Phase, "request_id": e.RequestID, "route": e.Route, "segments": e.Segments}, func(req *http.Request, body []byte) error {
			req.Header.Set("X-OLP-Signature", Signature([]byte(credential.SigningKey), time.Now(), body))
			return nil
		})
		if err != nil {
			return
		}
		err = decodeConfig(raw, &verdict)
		return
	}
	verdict = Verdict{Action: "pass"}
	missingReplacement := false
	for _, segment := range e.Segments {
		var part Verdict
		part, err = r.vendor(ctx, client, c, credential, d.Type, e.Phase, segment)
		if err != nil {
			return
		}
		if err = validateVerdict(part, []Segment{segment}); err != nil {
			return
		}
		if part.Action != "pass" {
			missingReplacement = missingReplacement || len(part.Redactions) == 0
			verdict.Action = Block
			for _, category := range part.Categories {
				verdict.Categories = appendCategory(verdict.Categories, category)
			}
			verdict.Redactions = append(verdict.Redactions, part.Redactions...)
		}
	}
	// A transformation is safe only if every detected segment is repaired.
	// Keep the finding, but omit partial repairs so the engine refuses it even
	// when the definition's service-error policy is fail_open.
	if missingReplacement {
		verdict.Redactions = nil
	}
	return
}

// Signature authenticates a timestamp and the exact transmitted bytes. Receivers
// should reject timestamps outside their replay window and compare MACs in
// constant time.
func Signature(key []byte, now time.Time, body []byte) string {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
func externalJSON(ctx context.Context, client *http.Client, endpoint string, body any, auth func(*http.Request, []byte) error) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > MaxContentBytes {
		return nil, errEvaluation
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, errEvaluation
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err = auth(req, encoded); err != nil {
		return nil, errEvaluation
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errEvaluation
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errEvaluation
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 || !json.Valid(raw) {
		return nil, errEvaluation
	}
	return raw, nil
}
func (r *ExternalRunner) vendor(ctx context.Context, client *http.Client, c ExternalConfig, credential externalCredential, kind, phase string, s Segment) (Verdict, error) {
	if vendor, ok := lookupNamedVendor(kind); ok {
		return vendor.evaluate(ctx, client, c, credential, phase, s)
	}
	endpoint := strings.TrimRight(c.Endpoint, "/")
	var body any
	auth := func(req *http.Request, _ []byte) error {
		if credential.APIKey == "" {
			return errEvaluation
		}
		req.Header.Set("Authorization", "Bearer "+credential.APIKey)
		return nil
	}
	switch kind {
	case "bedrock":
		endpoint += "/guardrail/" + url.PathEscape(c.Guardrail) + "/version/" + url.PathEscape(c.Version) + "/apply"
		source := "INPUT"
		if phase == Output || phase == OutputStream {
			source = "OUTPUT"
		}
		body = map[string]any{"source": source, "content": []any{map[string]any{"text": map[string]string{"text": s.Text}}}}
		auth = func(req *http.Request, encoded []byte) error {
			if credential.AccessKeyID == "" || credential.SecretAccessKey == "" {
				return errEvaluation
			}
			hash := sha256.Sum256(encoded)
			return v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: credential.AccessKeyID, SecretAccessKey: credential.SecretAccessKey, SessionToken: credential.SessionToken}, req, hex.EncodeToString(hash[:]), "bedrock", c.Region, time.Now())
		}
	case "azure.prompt_shields":
		endpoint += "/contentsafety/text:shieldPrompt?api-version=2024-09-01"
		body = map[string]any{"userPrompt": s.Text, "documents": []string{}}
		if s.Kind == "tool_result" {
			body = map[string]any{"documents": []string{s.Text}}
		}
	case "azure.content_safety":
		endpoint += "/contentsafety/text:analyze?api-version=2024-09-01"
		body = map[string]any{"text": s.Text, "outputType": "EightSeverityLevels"}
	case "model_armor":
		method := "sanitizeUserPrompt"
		field := "userPromptData"
		if phase == Output || phase == OutputStream {
			method = "sanitizeModelResponse"
			field = "modelResponseData"
		}
		endpoint += "/v1/" + c.Template + ":" + method
		body = map[string]any{field: map[string]string{"text": s.Text}}
	case "lakera":
		endpoint += "/v2/guard"
		role := s.Role
		if role != "system" && role != "assistant" && role != "tool" {
			role = "user"
		}
		body = map[string]any{"messages": []any{map[string]string{"role": role, "content": s.Text}}}
	case "presidio":
		endpoint += "/analyze"
		language := c.Language
		if language == "" {
			language = "en"
		}
		body = map[string]any{"text": s.Text, "language": language}
	default:
		return Verdict{}, errEvaluation
	}
	if strings.HasPrefix(kind, "azure.") {
		auth = func(req *http.Request, _ []byte) error {
			if credential.APIKey == "" {
				return errEvaluation
			}
			req.Header.Set("Ocp-Apim-Subscription-Key", credential.APIKey)
			return nil
		}
	}
	raw, err := externalJSON(ctx, client, endpoint, body, auth)
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Action: "pass"}
	block := func(category string) { v.Action = Block; v.Categories = appendCategory(v.Categories, category) }
	switch kind {
	case "bedrock":
		var response struct {
			Action  string `json:"action"`
			Outputs []struct {
				Text string `json:"text"`
			} `json:"outputs"`
		}
		if json.Unmarshal(raw, &response) != nil {
			return v, errEvaluation
		}
		switch response.Action {
		case "GUARDRAIL_INTERVENED":
			block("vendor.bedrock")
			if len(response.Outputs) == 1 && len(s.Text) > 0 {
				v.Redactions = []Redaction{{s.ID, 0, len(s.Text), response.Outputs[0].Text}}
			}
		case "NONE":
		default:
			return v, errEvaluation
		}
	case "azure.prompt_shields":
		var response struct {
			User *struct {
				Attack *bool `json:"attackDetected"`
			} `json:"userPromptAnalysis"`
			Documents []struct {
				Attack *bool `json:"attackDetected"`
			} `json:"documentsAnalysis"`
		}
		if json.Unmarshal(raw, &response) != nil || (s.Kind != "tool_result" && (response.User == nil || response.User.Attack == nil)) || (s.Kind == "tool_result" && len(response.Documents) != 1) {
			return v, errEvaluation
		}
		if response.User != nil && response.User.Attack != nil && *response.User.Attack {
			block("vendor.prompt_attack")
		}
		for _, d := range response.Documents {
			if d.Attack == nil {
				return v, errEvaluation
			}
			if *d.Attack {
				block("vendor.document_attack")
			}
		}
	case "azure.content_safety":
		var response struct {
			Categories []struct {
				Category string `json:"category"`
				Severity *int   `json:"severity"`
			} `json:"categoriesAnalysis"`
		}
		if json.Unmarshal(raw, &response) != nil || len(response.Categories) != 4 {
			return v, errEvaluation
		}
		seen := map[string]bool{}
		for _, categ := range response.Categories {
			if seen[categ.Category] || !slices.Contains([]string{"Hate", "SelfHarm", "Sexual", "Violence"}, categ.Category) || categ.Severity == nil || *categ.Severity < 0 || *categ.Severity > 7 {
				return v, errEvaluation
			}
			seen[categ.Category] = true
			if *categ.Severity > c.Threshold {
				block("vendor.content_safety")
			}
		}
	case "model_armor":
		var response struct {
			Result *struct {
				State      string `json:"filterMatchState"`
				Invocation string `json:"invocationResult"`
			} `json:"sanitizationResult"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Result == nil {
			return v, errEvaluation
		}
		if response.Result.Invocation != "" && response.Result.Invocation != "SUCCESS" {
			return v, errEvaluation
		}
		switch response.Result.State {
		case "MATCH_FOUND":
			block("vendor.model_armor")
		case "NO_MATCH_FOUND":
		default:
			return v, errEvaluation
		}
	case "lakera":
		var response struct {
			Flagged *bool `json:"flagged"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Flagged == nil {
			return v, errEvaluation
		}
		if *response.Flagged {
			block("vendor.lakera")
		}
	case "presidio":
		var response []struct {
			Type  string  `json:"entity_type"`
			Start int     `json:"start"`
			End   int     `json:"end"`
			Score float64 `json:"score"`
		}
		if json.Unmarshal(raw, &response) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return v, errEvaluation
		}
		for _, entity := range response {
			start, end := runeOffset(s.Text, entity.Start), runeOffset(s.Text, entity.End)
			if start < 0 || end <= start || entity.Type == "" || entity.Score < 0 || entity.Score > 1 {
				return v, errEvaluation
			}
			block("vendor.presidio")
			v.Redactions = append(v.Redactions, Redaction{s.ID, start, end, "[REDACTED]"})
		}
		v.Redactions = mergeRedactions(v.Redactions)
		if len(response) > 0 && c.AnonymizerEndpoint != "" {
			raw, err := externalJSON(ctx, client, strings.TrimRight(c.AnonymizerEndpoint, "/")+"/anonymize", map[string]any{"text": s.Text, "analyzer_results": response, "anonymizers": map[string]any{"DEFAULT": map[string]string{"type": "replace", "new_value": "[REDACTED]"}}}, auth)
			if err != nil {
				return v, err
			}
			var result struct {
				Text *string `json:"text"`
			}
			if json.Unmarshal(raw, &result) != nil || result.Text == nil || !utf8.ValidString(*result.Text) {
				return v, errEvaluation
			}
			v.Redactions = []Redaction{{s.ID, 0, len(s.Text), *result.Text}}
		}
	}
	return v, nil
}
func runeOffset(text string, index int) int {
	if index < 0 {
		return -1
	}
	offset := 0
	for i := 0; i < index; i++ {
		if offset >= len(text) {
			return -1
		}
		_, n := utf8.DecodeRuneInString(text[offset:])
		offset += n
	}
	return offset
}

// ModelVerdict decodes a configured JSON pointer without evaluating code or
// accepting a model-supplied action/category.
func ModelVerdict(raw []byte, rule ModelRule) (Verdict, error) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return Verdict{}, errEvaluation
	}
	if rule.Pointer != "" {
		if !strings.HasPrefix(rule.Pointer, "/") {
			return Verdict{}, errEvaluation
		}
		for _, part := range strings.Split(rule.Pointer[1:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			switch v := value.(type) {
			case map[string]any:
				var ok bool
				value, ok = v[part]
				if !ok {
					return Verdict{}, errEvaluation
				}
			case []any:
				i, err := strconv.Atoi(part)
				if err != nil || i < 0 || i >= len(v) {
					return Verdict{}, errEvaluation
				}
				value = v[i]
			default:
				return Verdict{}, errEvaluation
			}
		}
	}
	encoded, _ := json.Marshal(value)
	var expected any
	if json.Unmarshal(rule.Equals, &expected) != nil {
		return Verdict{}, errEvaluation
	}
	canonical, _ := json.Marshal(expected)
	if bytes.Equal(encoded, canonical) {
		return Verdict{Action: Block, Categories: []string{rule.Category}}, nil
	}
	return Verdict{Action: "pass"}, nil
}
