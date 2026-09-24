package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Profile links independently owned dialect, hosting and authentication contracts.
// Revision is OLP's immutable composition revision, not a provider model revision.
// Omitted profiles retain legacy configuration and published snapshot semantics.
type Profile struct {
	OperationDialects map[string]string          `json:"operation_dialects"`
	DefaultSchemas    map[string]json.RawMessage `json:"default_schemas"`
	ID                string                     `json:"id"`
	Revision          string                     `json:"revision"`
	Label             string                     `json:"label"`
	Kind              string                     `json:"kind"`
	Dialect           string                     `json:"dialect"`
	DialectRevision   string                     `json:"dialect_revision"`
	Hosting           string                     `json:"hosting"`
	Authentication    []string                   `json:"authentication"`
	Transport         string                     `json:"transport"`
	Operations        []string                   `json:"operations"`
	SemanticHeaders   []string                   `json:"semantic_headers"`
	QuerySettings     []string                   `json:"query_settings"`
	Documentation     string                     `json:"documentation"`
}

const ProfileRevision = "1"
const anthropicMessagesRevision = "2023-06-01"

var profileMu sync.RWMutex

var profileRegistry = []Profile{
	{ID: "openai-chat", Label: "OpenAI Chat Completions", Kind: "openai", Dialect: "openai-chat", Hosting: "direct-openai"},
	{ID: "openai-responses", Label: "OpenAI Responses", Kind: "openai", Dialect: "openai-responses", Hosting: "direct-openai"},
	{ID: "compatible-chat", Label: "Compatible Chat Completions", Kind: "openai_compatible", Dialect: "openai-chat", Hosting: "direct-compatible"},
	{ID: "compatible-responses", Label: "Compatible Responses", Kind: "openai_compatible", Dialect: "openai-responses", Hosting: "direct-compatible"},
	{ID: "anthropic-messages", Label: "Anthropic Messages", Kind: "anthropic", Dialect: "anthropic-messages", DialectRevision: anthropicMessagesRevision, Hosting: "direct-anthropic"},
	{ID: "gemini-generation", Label: "Gemini GenerateContent", Kind: "gemini", Dialect: "gemini-generate-content", DialectRevision: "v1beta", Hosting: "direct-gemini"},
	{ID: "gemini-interactions", Label: "Gemini Interactions", Kind: "gemini", Dialect: "gemini-interactions", DialectRevision: "v1beta", Hosting: "direct-gemini-interactions"},
	{ID: "gemini-live", Label: "Gemini Live", Kind: "gemini", Dialect: "gemini-live", DialectRevision: "v1beta", Hosting: "direct-gemini-live"},
	{ID: "azure-legacy-chat", Label: "Azure deployment Chat Completions", Kind: "azure_openai", Dialect: "openai-chat", Hosting: "azure-deployment"},
	{ID: "azure-legacy-responses", Label: "Azure legacy Responses", Kind: "azure_openai", Dialect: "openai-responses", Hosting: "azure-responses-legacy"},
	{ID: "azure-v1-chat", Label: "Azure v1 Chat Completions", Kind: "azure_openai", Dialect: "openai-chat", DialectRevision: "v1", Hosting: "azure-v1"},
	{ID: "azure-v1-responses", Label: "Azure v1 Responses", Kind: "azure_openai", Dialect: "openai-responses", DialectRevision: "v1", Hosting: "azure-v1"},
	{ID: "vertex-gemini", Label: "Vertex Google publisher", Kind: "vertex_ai", Dialect: "gemini-generate-content", DialectRevision: "v1", Hosting: "vertex-google"},
	{ID: "vertex-anthropic", Label: "Vertex Anthropic publisher", Kind: "vertex_ai", Dialect: "anthropic-messages", DialectRevision: "vertex-2023-10-16", Hosting: "vertex-anthropic"},
	{ID: "bedrock-converse", Label: "Bedrock Converse", Kind: "bedrock", Dialect: "bedrock-converse", Hosting: "bedrock-converse"},
	{ID: "bedrock-anthropic-invoke", Label: "Bedrock Anthropic Invoke", Kind: "bedrock", Dialect: "anthropic-messages", DialectRevision: "bedrock-2023-05-31", Hosting: "bedrock-anthropic-invoke"},
	{ID: "bedrock-invoke", Label: "Bedrock model-specific Invoke", Kind: "bedrock", Dialect: "bedrock-invoke", Hosting: "bedrock-invoke"},
}

func init() {
	for i := range profileRegistry {
		p := &profileRegistry[i]
		p.Revision, p.Transport = ProfileRevision, "http"
		p.Authentication = []string{"api_key", "headers", "none"}
		p.Operations = []string{"generation", "token_count"}
		p.SemanticHeaders, p.QuerySettings = []string{}, []string{}
		if p.DialectRevision == "" {
			p.DialectRevision = "unversioned-2026-09-22"
		}
		switch p.Kind {
		case "openai", "openai_compatible", "azure_openai":
			p.Operations = []string{"generation", "token_count", "embeddings", "moderation", "image_generation", "image_edit", "image_variation", "speech", "transcription", "translation", "video_create", "video_list", "video_get", "video_content", "video_delete"}
			p.SemanticHeaders = []string{"Openai-Beta"}
			p.Documentation = "https://developers.openai.com/api/docs/guides/migrate-to-responses"
			if p.Kind == "openai_compatible" {
				p.Operations = append(p.Operations, "rerank")
			}
			if p.Kind != "openai_compatible" {
				p.Operations = append(p.Operations, "batch", "realtime")
			}
			if p.Kind == "azure_openai" {
				p.Authentication = []string{"api_key", "azure_default", "azure_client_secret"}
				if p.Hosting != "azure-v1" {
					p.QuerySettings = []string{"api-version"}
				}
				p.Documentation = "https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle"
			}
		case "anthropic":
			p.SemanticHeaders = []string{"Anthropic-Version", "Anthropic-Beta"}
			p.Documentation = "https://platform.claude.com/docs/en/api/versioning"
		case "gemini", "vertex_ai":
			p.Documentation = "https://ai.google.dev/api/generate-content"
			p.QuerySettings = []string{"$xgafv"}
			p.Operations = []string{"generation", "token_count", "embeddings"}
			if p.Kind == "vertex_ai" {
				p.Authentication = []string{"adc", "service_account"}
				p.Operations = append(p.Operations, "image_generation")
			}
			if p.Hosting == "vertex-anthropic" {
				p.Operations = []string{"generation"}
				p.SemanticHeaders = []string{"Anthropic-Beta"}
				p.Documentation = "https://platform.claude.com/docs/en/build-with-claude/claude-on-vertex-ai"
			}
		case "bedrock":
			p.Authentication = []string{"static", "default_chain"}
			p.Documentation = "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html"
			p.Operations = []string{"generation", "token_count", "embeddings", "image_generation"}
			if p.Hosting == "bedrock-anthropic-invoke" || p.Hosting == "bedrock-invoke" {
				p.Operations = []string{"bedrock_invoke"}
				if p.Hosting == "bedrock-anthropic-invoke" {
					p.Operations = append(p.Operations, "generation")
				}
				p.Documentation = "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html"
			}
		}
		// Interactions and Live have independent request and event grammars.
		// Their profile tuples cannot fall through GenerateContent's codec.
		switch p.Dialect {
		case "gemini-interactions":
			p.Operations = []string{"generation"}
			p.Authentication = []string{"api_key"}
			p.Documentation = "https://ai.google.dev/gemini-api/docs/interactions-overview"
		case "gemini-live":
			p.Operations = []string{"realtime"}
			p.Authentication = []string{"api_key"}
			p.Transport = "websocket"
			p.Documentation = "https://ai.google.dev/api/live"
		}
		completeProfileMetadata(p)
	}
	registerUnaryProfiles()
}

// Profiles returns detached catalogue metadata; registering a new hosting profile
// using these components does not require a generation-kernel provider switch.
func Profiles() []Profile {
	profileMu.RLock()
	defer profileMu.RUnlock()
	out := make([]Profile, len(profileRegistry))
	for i, p := range profileRegistry {
		out[i] = cloneProfile(p)
	}
	return out
}

func cloneProfile(p Profile) Profile {
	p.OperationDialects = maps.Clone(p.OperationDialects)
	schemas := make(map[string]json.RawMessage, len(p.DefaultSchemas))
	for name, schema := range p.DefaultSchemas {
		schemas[name] = bytes.Clone(schema)
	}
	p.DefaultSchemas = schemas
	p.Authentication = slices.Clone(p.Authentication)
	p.Operations = slices.Clone(p.Operations)
	p.SemanticHeaders = slices.Clone(p.SemanticHeaders)
	p.QuerySettings = slices.Clone(p.QuerySettings)
	return p
}

// profileView borrows immutable catalogue metadata for connector methods that
// only read it. Registration detaches caller-owned maps and slices before
// publication; registered entries are never modified after publication.
func profileView(id, revision string) (Profile, error) {
	profileMu.RLock()
	defer profileMu.RUnlock()
	for _, p := range profileRegistry {
		if p.ID == id && p.Revision == revision {
			return p, nil
		}
	}
	return Profile{}, errors.New("unknown provider profile or unsupported profile revision")
}

func LookupProfile(id, revision string) (Profile, error) {
	p, err := profileView(id, revision)
	if err != nil {
		return Profile{}, err
	}
	return cloneProfile(p), nil
}

func (c Config) Profile() (Profile, error) {
	return LookupProfile(c.ProfileID, c.ProfileRevision)
}

func (c Config) Hosting() string {
	if p, err := profileView(c.ProfileID, c.ProfileRevision); err == nil {
		return p.Hosting
	}
	return ""
}

func (c Config) ValidateProfile() error {
	if c.ProfileID == "" {
		if c.ProfileRevision != "" || len(c.SemanticHeaders) > 0 || len(c.QuerySettings) > 0 || len(c.OperationDefaults) > 0 || len(c.Bindings) > 0 {
			return errors.New("semantic configuration and serving bindings require an explicit versioned profile")
		}
		return nil
	}
	p, err := profileView(c.ProfileID, c.ProfileRevision)
	if err != nil {
		return err
	}
	if p.Kind != c.Kind || !slices.Contains(p.Authentication, c.AuthMode) {
		return errors.New("profile, connector kind and authentication are not a supported composition")
	}
	if p.Dialect == "gemini-interactions" || p.Dialect == "gemini-live" {
		if len(c.OperationDefaults) > 0 {
			return errors.New("Gemini lifecycle profile does not admit unimplemented provider defaults")
		}
		for _, binding := range c.Bindings {
			if len(binding.Defaults) > 0 {
				return errors.New("Gemini lifecycle profile does not admit unimplemented model defaults")
			}
		}
	}
	if p.Dialect == "openai-responses" && slices.Contains([]string{"deepseek", "fireworks", "deepinfra", "huggingface", "perplexity", "cohere"}, c.VendorID) {
		return errors.New("this vendor's declared dialect does not include Responses")
	}
	if len(c.SemanticHeaders) > 16 || len(c.QuerySettings) > 16 {
		return errors.New("use at most 16 semantic headers and query settings")
	}
	seen := map[string]bool{}
	for name, value := range c.SemanticHeaders {
		key := textproto.CanonicalMIMEHeaderKey(name)
		if seen[key] || !slices.Contains(p.SemanticHeaders, key) || len(value) > 2048 || !httpguts.ValidHeaderFieldValue(value) {
			return errors.New("semantic header is duplicated, malformed or outside the profile allowlist")
		}
		seen[key] = true
		if slices.ContainsFunc(c.CredentialHeaders, func(h string) bool { return strings.EqualFold(h, key) }) {
			return errors.New("semantic headers cannot also be credential headers")
		}
		if key == "Anthropic-Version" && value != p.DialectRevision {
			return errors.New("Anthropic version must match the selected profile revision")
		}
	}
	for name, value := range c.QuerySettings {
		if name == "$xgafv" && value != "1" && value != "2" {
			return errors.New("Google $xgafv query value must be 1 or 2")
		}
		if name == "api-version" && value != c.APIVersion {
			return errors.New("api-version query must match the configured API revision")
		}
		if !slices.Contains(p.QuerySettings, name) || len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("query setting is outside the profile allowlist")
		}
	}
	for _, h := range c.CredentialHeaders {
		if slices.ContainsFunc(p.SemanticHeaders, func(v string) bool { return strings.EqualFold(v, h) }) {
			return errors.New("semantic/version headers belong in semantic_headers independently of credentials")
		}
	}
	return c.validateDefaultsAndBindings(p)
}

// TargetFamily is explicit for generation and operation-owned for other calls.
// The raw model-specific Invoke profile deliberately cannot enter a chat codec.
func (c Config) TargetFamily(source openai.Family) (openai.Family, error) {
	p, err := profileView(c.ProfileID, c.ProfileRevision)
	if err != nil {
		return "", err
	}
	operation := source.Operation()
	if source == openai.Family("bedrock_count") {
		operation = "token_count"
	}
	if !slices.Contains(p.Operations, operation) {
		return "", errors.New("operation is outside the selected provider profile")
	}
	switch operation {
	case "generation":
		switch p.Dialect {
		case "openai-chat":
			return openai.FamilyChat, nil
		case "openai-responses":
			return openai.FamilyResponses, nil
		case "anthropic-messages":
			return openai.FamilyAnthropic, nil
		case "gemini-generate-content":
			return openai.FamilyGemini, nil
		case "bedrock-converse":
			return openai.FamilyBedrock, nil
		}
	case "token_count":
		switch p.Dialect {
		case "anthropic-messages":
			return openai.FamilyAnthropicCount, nil
		case "gemini-generate-content":
			return openai.FamilyGeminiCount, nil
		case "bedrock-converse":
			return openai.Family("bedrock_count"), nil
		default:
			return openai.FamilyInputTokens, nil
		}
	case "embeddings":
		switch p.Hosting {
		case "direct-gemini":
			return openai.FamilyGeminiEmbeddings, nil
		case "vertex-google":
			return openai.FamilyVertexEmbeddings, nil
		case "bedrock-converse":
			return openai.FamilyBedrockEmbeddings, nil
		default:
			return openai.FamilyEmbeddings, nil
		}
	case "moderation":
		return openai.FamilyModeration, nil
	case "rerank":
		return openai.FamilyRerank, nil
	case "bedrock_invoke":
		return openai.FamilyBedrockInvoke, nil
	}
	return "", errors.New("operation has no codec in the selected profile")
}

func (c Config) Supports(operation, surface, mode string) bool {
	if c.ProfileID != "" {
		if p, err := profileView(c.ProfileID, c.ProfileRevision); err == nil {
			switch p.Dialect {
			case "gemini-interactions":
				return operation == "generation" && surface == "gemini" && (mode == "unary" || mode == "streaming")
			case "gemini-live":
				return operation == "realtime" && surface == "gemini" && mode == "realtime"
			}
			if codec, ok := operationregistry.Lookup(p.OperationDialect(operation)); ok && codec.Operation.ID == operation {
				if operationregistry.Default.SupportsTarget(codec.Identity, surface, mode) {
					return true
				}
				if _, dedicated := operationregistry.Lookup(p.Dialect); dedicated {
					return false
				}
				return Supports(c.Kind, c.VendorID, operation, surface, mode)
			}
		}
	}

	if !Supports(c.Kind, c.VendorID, operation, surface, mode) {
		return false
	}
	if c.ProfileID == "" {
		return true
	}
	p, err := profileView(c.ProfileID, c.ProfileRevision)
	return err == nil && slices.Contains(p.Operations, operation)
}

// ApplySemantic configures only profile-owned headers and query settings. Call
// before authentication so final SigV4 signs the complete request. Authentication
// never chooses the semantic configuration of an explicit profile.
func (c Config) ApplySemantic(req *http.Request) error {
	if c.ProfileID == "" {
		return nil
	}
	if err := c.ValidateProfile(); err != nil {
		return err
	}
	p, _ := profileView(c.ProfileID, c.ProfileRevision)
	if p.Hosting == "direct-anthropic" {
		req.Header.Set("Anthropic-Version", p.DialectRevision)
	}
	for name, value := range c.SemanticHeaders {
		req.Header.Set(name, value)
	}
	query := req.URL.Query()
	for name, value := range c.QuerySettings {
		if existing, found := query[name]; found && (len(existing) != 1 || existing[0] != value) {
			return fmt.Errorf("query setting %s collides with operation addressing", name)
		}
		query.Set(name, value)
	}
	req.URL.RawQuery = query.Encode()
	return nil
}

// BindIngressSemanticHeader validates the reviewed change of location from
// Anthropic's direct revision header to a cloud anthropic_version body tag.
// WrapBody writes the selected cloud tag before authentication/signing.
func (p Profile) BindIngressSemanticHeader(name, value string) (bool, error) {
	if !strings.EqualFold(name, "Anthropic-Version") || (p.Hosting != "vertex-anthropic" && p.Hosting != "bedrock-anthropic-invoke") {
		return false, nil
	}
	if value != anthropicMessagesRevision {
		return true, errors.New("the ingress API revision has no qualified hosting binding")
	}
	return true, nil
}

func (c Config) AzureScope() string {
	if c.Hosting() == "azure-v1" {
		return "https://ai.azure.com/.default"
	}
	return "https://cognitiveservices.azure.com/.default"
}

func (c Config) profileBase() string {
	base := strings.TrimRight(c.Endpoint, "/")
	if c.Hosting() == "azure-v1" && !strings.HasSuffix(base, "/openai/v1") {
		base += "/openai/v1"
	}
	return base
}

func (c Config) validateProfileEndpoint(u *url.URL) error {
	if (c.ProfileID == "cohere-embed-v2" || c.ProfileID == "cohere-rerank-v2") &&
		strings.EqualFold(u.Hostname(), "api.cohere.ai") && strings.TrimRight(u.Path, "/") != "/v2" {
		return errors.New("Cohere native v2 requires the /v2 endpoint; the compatibility/v1 preset is a separate API")
	}
	switch c.Hosting() {
	case "direct-gemini-interactions", "direct-gemini-live":
		if u.Path != "/v1beta" {
			return errors.New("Gemini lifecycle endpoint must end at /v1beta")
		}
		return nil
	case "azure-v1":
		if u.Path != "" && u.Path != "/openai/v1" {
			return errors.New("Azure v1 endpoint must be the resource origin or /openai/v1")
		}
		if c.APIVersion != "" || c.CloudProject != "" || c.CloudRegion != "" {
			return errors.New("Azure v1 does not use a dated api_version, project or region field")
		}
		if c.Deployment != "" && !ModelValid(c.Kind, c.Deployment) {
			return errors.New("Azure deployment is malformed")
		}
		return nil
	case "azure-responses-legacy":
		if u.Path != "" || c.CloudProject != "" || c.CloudRegion != "" {
			return errors.New("legacy Azure Responses requires a resource origin and API version")
		}
		if _, err := time.Parse("2006-01-02", strings.TrimSuffix(c.APIVersion, "-preview")); err != nil {
			return errors.New("legacy Azure Responses requires a dated API version")
		}
		return nil
	case "vertex-google", "vertex-anthropic":
		publisher := "google"
		if c.Hosting() == "vertex-anthropic" {
			publisher = "anthropic"
		}
		want := "/v1/projects/" + c.CloudProject + "/locations/" + c.CloudRegion + "/publishers/" + publisher
		if u.Path != want {
			return errors.New("Vertex endpoint path must match the profile publisher, project and location")
		}
	}
	return nil
}

// WrapBody handles only documented hosting wrappers after semantic lowering.
// Model and cloud revision construction precede authentication/signing.
func (c Config) WrapBody(body []byte, wire openai.Family) ([]byte, error) {
	hosting := c.Hosting()
	if hosting != "vertex-anthropic" && hosting != "bedrock-anthropic-invoke" {
		return body, nil
	}
	if wire != openai.FamilyAnthropic {
		return nil, errors.New("Anthropic cloud profile requires the Messages dialect")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("cloud request must be a JSON object")
	}
	p, _ := profileView(c.ProfileID, c.ProfileRevision)
	version, _ := json.Marshal(p.DialectRevision)
	if prior, found := fields["anthropic_version"]; found && string(prior) != string(version) {
		return nil, errors.New("native cloud version collides with the configured profile")
	}
	fields["anthropic_version"] = version
	delete(fields, "model")
	if hosting == "bedrock-anthropic-invoke" {
		delete(fields, "stream")
	}
	return json.Marshal(fields)
}

// OperationDialect names the operation schema instead of making non-generation
// defaults inherit a generation-shaped intermediate representation.
func (p Profile) OperationDialect(operation string) string {
	if dialect := p.OperationDialects[operation]; dialect != "" {
		return dialect
	}
	if operation == "generation" {
		return p.Dialect
	}
	switch operation {
	case "embeddings":
		switch p.Hosting {
		case "direct-gemini":
			return "gemini-embeddings"
		case "vertex-google":
			return "vertex-embeddings"
		case "bedrock-converse":
			return "bedrock-embeddings"
		}
		return "openai-embeddings"
	case "token_count":
		switch p.Dialect {
		case "anthropic-messages":
			return "anthropic-count-tokens"
		case "gemini-generate-content":
			return "gemini-count-tokens"
		case "bedrock-converse":
			return "bedrock-count-tokens"
		}
		return "openai-input-tokens"
	case "rerank":
		return "rerank"
	case "bedrock_invoke":
		return "bedrock-invoke"
	case "realtime":
		if (p.Kind == "openai" && p.Hosting == "direct-openai") || (p.Kind == "azure_openai" && p.Hosting == "azure-v1") {
			return "openai-realtime"
		}
	}
	return p.Hosting + "/" + operation
}

func completeProfileMetadata(p *Profile) {
	if p.OperationDialects == nil {
		p.OperationDialects = map[string]string{}
	}
	p.DefaultSchemas = map[string]json.RawMessage{}
	for _, operation := range p.Operations {
		dialect := p.OperationDialect(operation)
		p.OperationDialects[operation] = dialect
		fields := map[string]any{}
		for _, name := range defaultFields(dialect, operation) {
			fields[name] = defaultControlSchema(operation, name)
			if codec, ok := operationregistry.Lookup(dialect); ok {
				if field, found := codec.Defaults[name]; found {
					fields[name] = field.Schema
				}
			}
		}
		p.DefaultSchemas[operation], _ = json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": []string{"dialect"}, "properties": map[string]any{
			"dialect": map[string]any{"const": dialect}, "values": map[string]any{"type": "object", "additionalProperties": false, "properties": fields}, "native_options": map[string]any{"type": "object", "description": "Operation payload extensions; routing, credentials and resource references are reserved."},
		}})
	}
}

// RegisterProfile extends an existing trusted component composition at startup.
// A new provider using an existing dialect needs a profile, bindings and fixtures,
// not a provider case in the operation kernel. This is not a runtime plugin API.
func RegisterProfile(p Profile) error {
	if p.ID == "" || len(p.ID) > 128 || p.Revision == "" || p.Label == "" {
		return errors.New("profile identity, revision and label are required")
	}
	profileMu.Lock()
	defer profileMu.Unlock()
	var template *Profile
	for i := range profileRegistry {
		existing := &profileRegistry[i]
		if existing.ID == p.ID && existing.Revision == p.Revision {
			return errors.New("profile revision already registered")
		}
		if existing.Kind == p.Kind && existing.Dialect == p.Dialect && existing.Hosting == p.Hosting && existing.DialectRevision == p.DialectRevision {
			template = existing
		}
	}
	if template == nil || len(profileRegistry) >= 4096 {
		return errors.New("profile components do not form an existing registered composition")
	}
	for _, auth := range p.Authentication {
		if !slices.Contains(template.Authentication, auth) {
			return errors.New("profile authentication is incompatible")
		}
	}
	for _, operation := range p.Operations {
		if !slices.Contains(template.Operations, operation) {
			return errors.New("profile operation is incompatible")
		}
	}
	for _, header := range p.SemanticHeaders {
		if !slices.Contains(template.SemanticHeaders, header) {
			return errors.New("profile semantic header is incompatible")
		}
	}
	for _, query := range p.QuerySettings {
		if !slices.Contains(template.QuerySettings, query) {
			return errors.New("profile query setting is incompatible")
		}
	}
	if p.Transport != template.Transport || len(p.Authentication) == 0 || len(p.Operations) == 0 {
		return errors.New("profile transport and capabilities are required")
	}
	completeProfileMetadata(&p)
	profileRegistry = append(profileRegistry, cloneProfile(p))
	return nil
}

func defaultControlSchema(operation, name string) map[string]any {
	kind := "string"
	switch name {
	case "temperature", "top_p", "frequency_penalty", "presence_penalty", "speed":
		kind = "number"
	case "max_tokens", "max_completion_tokens", "max_output_tokens", "dimensions", "output_dimension", "n", "top_n", "top_k":
		kind = "integer"
	case "parallel_tool_calls", "logprobs", "return_documents", "autoTruncate", "normalize":
		kind = "boolean"
	case "tools", "safetySettings", "stop_sequences", "embeddingTypes", "additionalModelResponseFieldPaths", "timestamp_granularities":
		kind = "array"
	case "response_format", "reasoning", "text", "thinking", "output_config", "generationConfig", "toolConfig", "inferenceConfig", "additionalModelRequestFields", "parameters", "stream_options":
		kind = "object"
	}
	if name == "response_format" && operation != "generation" {
		kind = "string"
	}
	schema := map[string]any{"title": name, "type": []string{kind, "null"}}
	if operation == "translation" {
		if name == "response_format" {
			schema["enum"] = []any{"json", "text", "srt", "vtt", "verbose_json", nil}
		}
		if name == "temperature" {
			schema["minimum"], schema["maximum"] = 0, 1
		}
	}
	if kind == "array" || kind == "object" {
		schema["description"] = "Replaced atomically; never recursively merged."
	}
	if kind == "integer" {
		schema["minimum"] = 1
	}
	return schema
}
