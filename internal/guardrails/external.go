package guardrails

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails/vendor"
	pluginidentity "github.com/tyk-swe/olp/internal/plugins/identity"
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
	if _, ok := vendorInfo(d.Type); ok {
		_, err := vendor.Parse(vendor.Type(d.Type), d.Configuration)
		return err
	}
	if d.Type == "webhook" {
		_, err := vendor.ParseConnection(d.Configuration)
		return err
	}
	var c ExternalConfig
	if err := decodeConfig(d.Configuration, &c); err != nil {
		return err
	}
	switch d.Type {
	case "plugin":
		if !identifier.MatchString(c.PluginID) || !pluginidentity.ValidDigest(c.PluginVersion) {
			return errors.New("pin an approved plugin version")
		}
		return nil
	case "semantic_guard":
		if !identifier.MatchString(c.Route) || c.Rule != nil {
			return errors.New("semantic guardrails require an embedding route and semantic rule")
		}
		return ValidateSemantic(c.Semantic)
	case "model", "decision_model":
		if !identifier.MatchString(c.Route) || c.Rule == nil || !categoryID.MatchString(c.Rule.Category) || !json.Valid(c.Rule.Equals) || len(c.Rule.Pointer) > 512 || len(c.Rule.Prompt) > 8192 {
			return errors.New("invalid model guardrail route or verdict rule")
		}
		return nil
	default:
		return errors.New("unknown guardrail type")
	}
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
	definition := Revision{
		LegacyRuleID: d.definition.LegacyRuleID, ID: d.definition.ID, GuardrailID: d.definition.GuardrailID,
		ProjectID: d.definition.ProjectID, Name: d.definition.Name, Type: d.definition.Type,
		Configuration: bytes.Clone(d.definition.Configuration), TimeoutMS: d.definition.TimeoutMS, OnError: d.definition.OnError,
	}
	return d.evaluator.Evaluate(ctx, definition, e)
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
	vendor.Credential
	SigningKey string `json:"signing_key"`
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
	if d.TimeoutMS < 1 || d.TimeoutMS > 300000 || validateSegments(e.Segments) != nil || !e.Phase.Valid() {
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
	return r.dispatch(ctx, d, e)
}

func (r *ExternalRunner) dispatch(ctx context.Context, d Revision, e Evaluation) (Verdict, error) {
	switch {
	case isModelGuardrail(d.Type):
		return delegate(ctx, r.Model, d, e)
	case d.Type == "plugin":
		return delegate(ctx, r.Plugin, d, e)
	case d.Type == "webhook":
		c, err := vendor.ParseConnection(d.Configuration)
		if err != nil {
			return Verdict{}, err
		}
		client, credential, err := r.connect(ctx, d, c, c.Endpoint)
		if err != nil {
			return Verdict{}, err
		}
		return webhook(ctx, client, c.Endpoint, credential.SigningKey, d, e)
	}
	spec, err := vendor.Parse(vendor.Type(d.Type), d.Configuration)
	if err != nil {
		return Verdict{}, err
	}
	if info, _ := vendorInfo(d.Type); !info.Supports(e.Phase) {
		return Verdict{}, errEvaluation
	}
	client, credential, err := r.connect(ctx, d, spec.Connection, spec.Endpoints()...)
	if err != nil {
		return Verdict{}, err
	}
	return vendorVerdict(ctx, spec, client, credential.Credential, e)
}

func delegate(ctx context.Context, evaluator Evaluator, d Revision, e Evaluation) (Verdict, error) {
	if evaluator == nil {
		return Verdict{}, errEvaluation
	}
	return evaluator.Evaluate(ctx, d, e)
}

func (r *ExternalRunner) connect(ctx context.Context, d Revision, c vendor.Connection, endpoints ...string) (*http.Client, externalCredential, error) {
	for _, endpoint := range endpoints {
		if _, err := r.Egress.ValidateEndpoint(endpoint); err != nil {
			return nil, externalCredential{}, err
		}
	}
	if err := r.Egress.ValidateConnection(c.Network); err != nil {
		return nil, externalCredential{}, err
	}
	if r.Credentials == nil || r.Clients == nil {
		return nil, externalCredential{}, errEvaluation
	}
	secret, err := r.Credentials(ctx, c.CredentialRef)
	if err != nil {
		return nil, externalCredential{}, err
	}
	var credential externalCredential
	if err := json.Unmarshal(secret, &credential); err != nil {
		return nil, externalCredential{}, err
	}
	network, err := r.networkSecret(ctx, c.Network)
	if err != nil {
		return nil, externalCredential{}, err
	}
	client, err := r.Clients.ClientScoped("guardrail/"+d.GuardrailID+"/"+d.ID, r.Egress, c.Network, network, time.Duration(d.TimeoutMS)*time.Millisecond)
	return client, credential, err
}

func (r *ExternalRunner) networkSecret(ctx context.Context, network *egress.ConnectionOptions) ([]byte, error) {
	if network == nil || network.CredentialID == "" {
		return nil, nil
	}
	if r.NetworkCredentials == nil {
		return nil, errEvaluation
	}
	return r.NetworkCredentials(ctx, network.CredentialID)
}

func webhook(ctx context.Context, client *http.Client, endpoint, key string, d Revision, e Evaluation) (Verdict, error) {
	if len(key) < 32 {
		return Verdict{}, errEvaluation
	}
	body := map[string]any{"guardrail": d.Name, "phase": e.Phase, "request_id": e.RequestID, "route": e.Route, "segments": e.Segments}
	raw, err := vendor.PostJSON(ctx, client, endpoint, body, func(req *http.Request, encoded []byte) error {
		req.Header.Set("X-OLP-Signature", Signature([]byte(key), time.Now(), encoded))
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	var verdict Verdict
	if err := decodeConfig(raw, &verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

type finding struct {
	segment  string
	decision vendor.Decision
}

func vendorVerdict(ctx context.Context, spec vendor.Spec, client *http.Client, credential vendor.Credential, e Evaluation) (Verdict, error) {
	var findings []finding
	for _, s := range e.Segments {
		decision, err := spec.Call(ctx, client, credential, vendorRequest(e.Phase, s))
		if err != nil {
			return Verdict{}, err
		}
		if decision.Flagged() {
			findings = append(findings, finding{segment: s.ID, decision: decision})
		}
	}
	if len(findings) == 0 {
		return Verdict{Action: Pass}, nil
	}
	return Verdict{Action: Block, Categories: findingCategories(findings), Redactions: completeRepairs(findings)}, nil
}

func vendorRequest(phase Phase, s Segment) vendor.Request {
	return vendor.Request{Output: OutputPhase(phase), Document: s.Kind == ToolResult, Role: vendorRole(phase, s.Role), Text: s.Text}
}

func vendorRole(phase Phase, role Role) vendor.Role {
	switch {
	case OutputPhase(phase) || role == Assistant:
		return vendor.Assistant
	case role == System || role == Developer:
		return vendor.System
	default:
		return vendor.User
	}
}

func findingCategories(findings []finding) []string {
	var categories []string
	for _, f := range findings {
		for _, category := range f.decision.Categories() {
			categories = appendCategory(categories, category)
		}
	}
	return categories
}

func completeRepairs(findings []finding) []Redaction {
	var redactions []Redaction
	for _, f := range findings {
		spans := f.decision.Spans()
		if len(spans) == 0 {
			return nil
		}
		for _, span := range spans {
			redactions = append(redactions, Redaction{Segment: f.segment, Start: span.Start, End: span.End, Replacement: span.Replacement})
		}
	}
	return mergeRedactions(redactions)
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
	return Verdict{Action: Pass}, nil
}
