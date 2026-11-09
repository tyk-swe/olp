package vendor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Type = contract.CreateGuardrailRequestType

type Phase = contract.GuardrailPolicyEntryPhase

type Role string

const (
	System    Role = "system"
	User      Role = "user"
	Assistant Role = "assistant"
)

type Request struct {
	Output   bool
	Document bool
	Role     Role
	Text     string
}

type Span struct {
	Start       int
	End         int
	Replacement string
}

type Decision struct {
	categories []string
	spans      []Span
}

func (d Decision) Flagged() bool        { return len(d.categories) > 0 }
func (d Decision) Categories() []string { return slices.Clone(d.categories) }
func (d Decision) Spans() []Span        { return slices.Clone(d.spans) }

type Credential struct {
	APIKey          string `json:"api_key"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

type Connection struct {
	Endpoint      string                    `json:"endpoint"`
	CredentialRef string                    `json:"credential_ref"`
	Network       *egress.ConnectionOptions `json:"network,omitempty"`
	ContentEgress bool                      `json:"content_egress"`
}

func ParseConnection(raw json.RawMessage) (Connection, error) {
	var c Connection
	if err := decode(raw, &c); err != nil {
		return Connection{}, err
	}
	return c, c.validate()
}

func (c Connection) connection() Connection { return c }
func (c Connection) urls() []string         { return []string{c.Endpoint} }

func (c Connection) validate() error {
	if !c.ContentEgress || !validURL(c.Endpoint) {
		return errors.New("external guardrails require an endpoint and explicit content egress acknowledgement")
	}
	if len(c.CredentialRef) < 1 || !bounded(c.CredentialRef, 100) {
		return errors.New("external guardrails require a credential reference")
	}
	return nil
}

type Spec struct {
	Connection
	endpoints []string
	call      func(context.Context, exchange, Request) (Decision, error)
}

func (s Spec) Endpoints() []string { return slices.Clone(s.endpoints) }

func (s Spec) Call(ctx context.Context, client *http.Client, credential Credential, r Request) (Decision, error) {
	return s.call(ctx, exchange{client: client, endpoint: s.Endpoint, credential: credential}, r)
}

type Info struct {
	Transforms bool
	Compresses bool
	phases     []Phase
}

func (i Info) Supports(phase Phase) bool {
	return len(i.phases) == 0 || slices.Contains(i.phases, phase)
}

type vendor struct {
	Info
	parse func(json.RawMessage) (Spec, error)
}

var (
	inputs      = []Phase{contract.GuardrailPolicyEntryPhaseInput, contract.GuardrailPolicyEntryPhaseInputParallel}
	synchronous = []Phase{contract.GuardrailPolicyEntryPhaseInput}
)

var vendors = map[Type]vendor{
	"aim":                   {Info{Transforms: true}, plain(aim)},
	"akto":                  {Info{}, named(aktoParameters.valid, akto)},
	"alice":                 {Info{Transforms: true}, plain(alice)},
	"aporia":                {Info{}, plain(aporia)},
	"azure.content_safety":  {Info{}, configured(contentSafetyConfig.check, contentSafety)},
	"azure.prompt_shields":  {Info{}, configured(Connection.check, promptShields)},
	"bedrock":               {Info{Transforms: true}, configured(bedrockConfig.check, bedrock)},
	"cato_networks":         {Info{Transforms: true}, plain(cato)},
	"cisco_ai_defense":      {Info{Transforms: true}, named(ciscoParameters.valid, cisco)},
	"compresr":              {Info{Transforms: true, Compresses: true, phases: synchronous}, named(compresrParameters.valid, compresr)},
	"conduct":               {Info{phases: inputs}, named(conductParameters.valid, conduct)},
	"crowdstrike_aidr":      {Info{Transforms: true}, named(crowdstrikeParameters.valid, crowdstrike)},
	"deepkeep":              {Info{Transforms: true}, named(deepkeepParameters.valid, deepkeep)},
	"dynamoai":              {Info{}, named(dynamoParameters.valid, dynamo)},
	"enkryptai":             {Info{}, named(enkryptParameters.valid, enkrypt)},
	"generic_guardrail_api": {Info{Transforms: true}, plain(genericGuardrail)},
	"grayswan":              {Info{}, named(grayswanParameters.valid, grayswan)},
	"guardrails_ai":         {Info{Transforms: true}, plain(guardrailsAI)},
	"headroom":              {Info{Transforms: true, Compresses: true, phases: synchronous}, named(headroomParameters.valid, headroom)},
	"hiddenlayer":           {Info{Transforms: true}, configured(hiddenlayerConfig.check, hiddenlayer)},
	"ibm_guardrails":        {Info{}, named(ibmParameters.valid, ibm)},
	"javelin":               {Info{phases: inputs}, named(javelinParameters.valid, javelin)},
	"lakera":                {Info{}, configured(Connection.check, lakera)},
	"lakera_v1":             {Info{phases: inputs}, named(lakeraParameters.valid, lakeraV1)},
	"lasso":                 {Info{Transforms: true}, named(lassoParameters.valid, lasso)},
	"microsoft_purview":     {Info{}, named(purviewParameters.valid, purview)},
	"model_armor":           {Info{}, configured(modelArmorConfig.check, modelArmor)},
	"noma":                  {Info{Transforms: true}, named(nomaParameters.validV1, noma)},
	"noma_v2":               {Info{Transforms: true}, named(nomaParameters.valid, nomaV2)},
	"onyx":                  {Info{}, plain(onyx)},
	"openai_moderation":     {Info{}, named(moderationParameters.valid, openAIModeration)},
	"ovalix":                {Info{Transforms: true}, named(ovalixParameters.valid, ovalix)},
	"pangea":                {Info{Transforms: true}, named(pangeaParameters.valid, pangea)},
	"panw_prisma_airs":      {Info{Transforms: true}, named(panwParameters.valid, panw)},
	"pillar":                {Info{Transforms: true}, named(pillarParameters.valid, pillar)},
	"presidio":              {Info{Transforms: true}, configured(presidioConfig.check, presidio)},
	"prompt_security":       {Info{Transforms: true}, named(promptSecurityParameters.valid, promptSecurity)},
	"promptguard":           {Info{Transforms: true}, plain(promptGuard)},
	"qostodian_nexus":       {Info{Transforms: true}, named(qostodianParameters.valid, qostodian)},
	"qualifire":             {Info{}, named(qualifireParameters.valid, qualifire)},
	"repelloai":             {Info{}, named(repelloParameters.valid, repello)},
	"rubrik":                {Info{}, plain(rubrik)},
	"singulr":               {Info{}, named(singulrParameters.valid, singulr)},
	"straiker":              {Info{Transforms: true}, named(straikerParameters.valid, straiker)},
	"vigil_guard":           {Info{Transforms: true}, plain(vigil)},
	"xecguard":              {Info{}, named(xecguardParameters.valid, xecguard)},
	"zscaler_ai_guard":      {Info{}, named(zscalerParameters.valid, zscaler)},
}

func Lookup(t Type) (Info, bool) {
	v, ok := vendors[t]
	return v.Info, ok
}

func Parse(t Type, raw json.RawMessage) (Spec, error) {
	v, ok := vendors[t]
	if !ok {
		return Spec{}, errors.New("unknown guardrail type")
	}
	return v.parse(raw)
}

type config interface {
	connection() Connection
	urls() []string
}

func configured[C config](check func(C) error, call func(context.Context, exchange, C, Request) (Decision, error)) func(json.RawMessage) (Spec, error) {
	return func(raw json.RawMessage) (Spec, error) {
		var c C
		if err := decode(raw, &c); err != nil {
			return Spec{}, err
		}
		if err := c.connection().validate(); err != nil {
			return Spec{}, err
		}
		if err := check(c); err != nil {
			return Spec{}, err
		}
		return Spec{
			Connection: c.connection(),
			endpoints:  c.urls(),
			call: func(ctx context.Context, x exchange, r Request) (Decision, error) {
				return call(ctx, x, c, r)
			},
		}, nil
	}
}

func (Connection) check() error { return nil }

type parameterized[P any] struct {
	Connection
	Parameters P `json:"parameters"`
}

func named[P any](valid func(P) bool, call func(context.Context, exchange, P, Request) (Decision, error)) func(json.RawMessage) (Spec, error) {
	return configured(func(c parameterized[P]) error {
		if !valid(c.Parameters) {
			return errors.New("invalid guardrail vendor parameters")
		}
		return nil
	}, func(ctx context.Context, x exchange, c parameterized[P], r Request) (Decision, error) {
		return call(ctx, x, c.Parameters, r)
	})
}

func plain(call func(context.Context, exchange, Request) (Decision, error)) func(json.RawMessage) (Spec, error) {
	return named(func(struct{}) bool { return true }, func(ctx context.Context, x exchange, _ struct{}, r Request) (Decision, error) {
		return call(ctx, x, r)
	})
}

var errUnavailable = errors.New("guardrail vendor unavailable")

type exchange struct {
	client     *http.Client
	endpoint   string
	credential Credential
}

func (x exchange) at(endpoint string) exchange {
	return exchange{client: x.client, endpoint: endpoint, credential: x.credential}
}

func (x exchange) apiKey() (string, error) {
	if x.credential.APIKey == "" || strings.ContainsAny(x.credential.APIKey, "\r\n\x00") {
		return "", errUnavailable
	}
	return x.credential.APIKey, nil
}

func (x exchange) post(ctx context.Context, body any, auth func(*http.Request, []byte) error) ([]byte, error) {
	return PostJSON(ctx, x.client, x.endpoint, body, auth)
}

func (x exchange) withKey(ctx context.Context, body any, headers func(http.Header, string)) ([]byte, error) {
	key, err := x.apiKey()
	if err != nil {
		return nil, err
	}
	return x.post(ctx, body, func(req *http.Request, _ []byte) error {
		headers(req.Header, key)
		return nil
	})
}

func (x exchange) keyed(ctx context.Context, body any, header, prefix string) ([]byte, error) {
	return x.withKey(ctx, body, func(h http.Header, key string) { h.Set(header, prefix+key) })
}

const (
	maxRequestBytes  = 8 << 20
	maxResponseBytes = 1 << 20
)

func PostJSON(ctx context.Context, client *http.Client, endpoint string, body any, auth func(*http.Request, []byte) error) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > maxRequestBytes {
		return nil, errUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, errUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if auth(req, encoded) != nil {
		return nil, errUnavailable
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errUnavailable
	}
	return readJSON(resp.Body)
}

func readJSON(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes || !json.Valid(raw) {
		return nil, errUnavailable
	}
	return raw, nil
}

func decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 || len(raw) > 128<<10 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return errors.New("invalid guardrail configuration")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("invalid guardrail configuration: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid guardrail configuration")
	}
	return nil
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 2048
}

func bounded(value string, limit int) bool {
	return len(value) <= limit && !strings.ContainsAny(value, "\r\n\x00")
}

func required(value string) bool { return value != "" && bounded(value, 256) }

func optional(value string) bool { return value == "" || required(value) }

func names(values []string, limit int) bool {
	return len(values) <= 64 && !slices.ContainsFunc(values, func(v string) bool { return v == "" || !bounded(v, limit) })
}

func flag(categories ...string) Decision { return Decision{categories: categories} }

func flagIf(flagged bool, category string) Decision {
	if !flagged {
		return Decision{}
	}
	return flag(category)
}

func replace(r Request, text, category string) (Decision, error) {
	if !utf8.ValidString(text) || len(text) > 4096 || r.Text == "" {
		return Decision{}, errUnavailable
	}
	if text == r.Text {
		return flag(category), nil
	}
	return Decision{categories: []string{category}, spans: []Span{{Start: 0, End: len(r.Text), Replacement: text}}}, nil
}

func pick(r Request, input, output string) string {
	if r.Output {
		return output
	}
	return input
}

func direction(r Request) string { return pick(r, "request", "response") }

func message(r Request) map[string]string {
	return map[string]string{"role": string(r.Role), "content": r.Text}
}

func messages(r Request) []map[string]string { return []map[string]string{message(r)} }

func label(value string) bool { return utf8.ValidString(value) && bounded(value, 512) }

func labels(values []string) bool {
	return len(values) <= 64 && !slices.ContainsFunc(values, func(v string) bool { return v == "" || !label(v) })
}

func failed(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func allowBlock(action, category string) (Decision, error) {
	switch action {
	case "allow":
		return Decision{}, nil
	case "block":
		return flag(category), nil
	default:
		return Decision{}, errUnavailable
	}
}
