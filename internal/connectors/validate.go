package connectors

import (
	"errors"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

func (c Config) ValidateProfile() error {
	if c.Plugin == nil && len(c.PluginOptions) > 0 {
		return errors.New("only plugin profiles declare options")
	}
	if c.ProfileID == "" {
		return c.validateUnprofiledSettings()
	}
	p, err := c.profile()
	if err != nil {
		return err
	}
	if c.Plugin != nil {
		if err = c.Plugin.ValidateOptions(c.PluginOptions); err != nil {
			return err
		}
	}
	if p.Kind != c.Kind || !slices.Contains(p.Authentication, c.AuthMode) {
		return errors.New("profile, connector kind and authentication are not a supported composition")
	}
	for _, check := range []func(Profile) error{
		c.validateProfileDefaultsAllowed,
		c.validateProfileVendor,
		c.validateSemanticHeaders,
		c.validateQuerySettings,
		c.validateCredentialHeaderConflicts,
		c.validateDefaultsAndBindings,
	} {
		if err := check(p); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) validateUnprofiledSettings() error {
	if c.ProfileRevision != "" || len(c.SemanticHeaders) > 0 || len(c.QuerySettings) > 0 || len(c.OperationDefaults) > 0 || len(c.Bindings) > 0 {
		return errors.New("semantic configuration and serving bindings require an explicit versioned profile")
	}
	return nil
}

func (c Config) validateProfileDefaultsAllowed(p Profile) error {
	if c.RetrievalBackend() != "" && (len(c.OperationDefaults) > 0 || len(c.Bindings) > 0) {
		return errors.New("retrieval profiles take resource and filter configuration from their explicit retrieval bindings")
	}
	switch p.Dialect {
	case "gemini-interactions", "gemini-live", "gemini-agents":
		return c.forbidProfileDefaults(
			"Gemini lifecycle profile does not admit unimplemented provider defaults",
			"Gemini lifecycle profile does not admit unimplemented model defaults",
		)
	case "xai-realtime", "vertex-realtime", "bedrock-realtime":
		return c.forbidProfileDefaults(
			"native duplex profile does not admit provider defaults on opaque frames",
			"native duplex profile does not admit model defaults on opaque frames",
		)
	}
	return nil
}

func (c Config) forbidProfileDefaults(providerMessage, modelMessage string) error {
	if len(c.OperationDefaults) > 0 {
		return errors.New(providerMessage)
	}
	for _, binding := range c.Bindings {
		if len(binding.Defaults) > 0 {
			return errors.New(modelMessage)
		}
	}
	return nil
}

func (c Config) validateProfileVendor(p Profile) error {
	if required, pinned := p.reviewedVendor(); pinned && c.VendorID != required.id {
		return errors.New(required.family + " profile requires its reviewed vendor")
	}
	if _, generation := generationFamily(p.Dialect); p.Dialect != "openai-completions" && generation && !vendors.Speaks(c.VendorID, p.Dialect) {
		return errors.New("the vendor's reviewed contract does not include the profile's dialect")
	}
	return nil
}

func (c Config) validateSemanticHeaders(p Profile) error {
	if len(c.SemanticHeaders) > 16 || len(c.QuerySettings) > 16 {
		return errors.New("use at most 16 semantic headers and query settings")
	}
	seen := make(map[string]bool, len(c.SemanticHeaders))
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
	return nil
}

func (c Config) validateQuerySettings(p Profile) error {
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
	return nil
}

func (c Config) validateCredentialHeaderConflicts(p Profile) error {
	for _, h := range c.CredentialHeaders {
		if slices.ContainsFunc(p.SemanticHeaders, func(v string) bool { return strings.EqualFold(v, h) }) {
			return errors.New("semantic/version headers belong in semantic_headers independently of credentials")
		}
	}
	return nil
}

func (c Config) validateProfileEndpoint(u *url.URL) error {
	if c.ProfileID == "gemini-decisions" && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1beta") {
		return errors.New("Gemini Decisions endpoint must end at /v1beta")
	}
	if (c.ProfileID == "cohere-embed-v2" || c.ProfileID == "cohere-rerank-v2" || c.ProfileID == "cohere-v2") &&
		strings.EqualFold(u.Hostname(), "api.cohere.ai") && strings.TrimRight(u.Path, "/") != "/v2" {
		return errors.New("Cohere native v2 requires the /v2 endpoint; the compatibility/v1 preset is a separate API")
	}
	if traits := c.traits(); c.ProfileID != "" && traits.googleService != "" {
		if u.Path != "/v1/"+vertexResource(c.CloudProject, c.CloudRegion)+traits.vertexPath {
			return errors.New(traits.misplaced)
		}
		return nil
	}
	return c.validateHostingEndpoint(u)
}

func (c Config) validateHostingEndpoint(u *url.URL) error {
	switch c.Hosting() {
	case "bedrock-knowledge-bases":
		if u.Path != "" {
			return errors.New("Knowledge Bases endpoint must be a service origin")
		}
	case pluginHosting:
		if c.Endpoint != c.Plugin.Address(c.PluginOptions) {
			return errors.New("a plugin provider's endpoint is its profile's address, with its options in place")
		}
	case "direct-gemini-interactions", "direct-gemini-live":
		if u.Path != "/v1beta" {
			return errors.New("Gemini lifecycle endpoint must end at /v1beta")
		}
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
	case "azure-responses-legacy":
		if u.Path != "" || c.CloudProject != "" || c.CloudRegion != "" {
			return errors.New("legacy Azure Responses requires a resource origin and API version")
		}
		if _, err := time.Parse("2006-01-02", strings.TrimSuffix(c.APIVersion, "-preview")); err != nil {
			return errors.New("legacy Azure Responses requires a dated API version")
		}
	}
	return nil
}

func (c Config) Validate(policy *egress.Policy) error {
	if err := policy.ValidateConnection(c.Network); err != nil {
		return err
	}
	if err := c.ValidateProfile(); err != nil {
		return err
	}
	u, err := policy.ValidateEndpoint(c.Endpoint)
	if err != nil {
		return err
	}
	if c.Kind == KindVertexAI && !c.vertexDestination(u) {
		return errors.New("Vertex credentials require the Google endpoint for the configured location and system trust roots")
	}
	if err := c.validateProfileEndpoint(u); err != nil {
		return err
	}
	if hosting := c.Hosting(); hosting == "azure-v1" || hosting == "azure-responses-legacy" {
		return nil
	}
	if validateKind, ok := kindValidators[c.Kind]; ok {
		return validateKind(c, u)
	}
	return validateNoCloudContext(c)
}

var kindValidators = map[Kind]func(Config, *url.URL) error{
	KindAzureAISearch: validateAzureAISearch,
	KindAzureOpenAI:   validateAzureOpenAI,
	KindVertexAI:      validateVertexAI,
	KindSageMaker:     validateSageMaker,
	KindWatsonx:       validateWatsonx,
	KindBedrock:       validateBedrock,
}

func validateAzureAISearch(c Config, u *url.URL) error {
	if c.ProfileID != "azure-ai-search" || u.Path != "" || c.CloudRegion != "" || c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "2025-09-01" {
		return errors.New("Azure AI Search requires its dedicated profile, a service origin and api_version 2025-09-01")
	}
	return nil
}

func validateAzureOpenAI(c Config, u *url.URL) error {
	if u.Path != "" {
		return errors.New("Azure endpoint must be a resource origin")
	}
	if !ModelValid(c.Kind, c.Deployment) {
		return errors.New("Azure deployment must be a valid deployment identifier")
	}
	version := strings.TrimSuffix(c.APIVersion, "-preview")
	date, err := time.Parse("2006-01-02", version)
	if err != nil || date.Year() < 2020 {
		return errors.New("Azure api_version must be a valid YYYY-MM-DD or YYYY-MM-DD-preview")
	}
	if c.CloudRegion != "" || c.CloudProject != "" {
		return errors.New("Azure uses endpoint, deployment and API version")
	}
	return nil
}

func validateVertexAI(c Config, _ *url.URL) error {
	if !cloudIdentifier.MatchString(c.CloudRegion) || !cloudIdentifier.MatchString(c.CloudProject) {
		return errors.New("Vertex requires a project and location")
	}
	if c.Deployment != "" || c.APIVersion != "" {
		return errors.New("Vertex deployment and API version belong in the native endpoint")
	}
	return nil
}

func validateSageMaker(c Config, u *url.URL) error {
	if !cloudIdentifier.MatchString(c.CloudRegion) {
		return errors.New("SageMaker requires an AWS region")
	}
	if u.Path != "" || c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "" {
		return errors.New("SageMaker uses a runtime endpoint origin and region; models name the endpoint")
	}
	return nil
}

func validateWatsonx(c Config, u *url.URL) error {
	if !cloudIdentifier.MatchString(c.CloudRegion) || !cloudIdentifier.MatchString(c.CloudProject) {
		return errors.New("watsonx requires a region and a project ID")
	}
	if u.Path != "" || c.Deployment != "" {
		return errors.New("watsonx uses a regional endpoint origin, region and project")
	}
	if date, err := time.Parse(time.DateOnly, c.APIVersion); c.APIVersion != "" && (err != nil || date.Year() < 2024) {
		return errors.New("watsonx api_version must be a YYYY-MM-DD date")
	}
	return nil
}

func validateBedrock(c Config, _ *url.URL) error {
	if !cloudIdentifier.MatchString(c.CloudRegion) {
		return errors.New("Bedrock requires an AWS region")
	}
	if c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "" {
		return errors.New("Bedrock uses endpoint and region")
	}
	return nil
}

func validateNoCloudContext(c Config) error {
	if c.CloudRegion != "" || c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "" {
		return errors.New("Cloud context applies only to cloud connectors")
	}
	return nil
}
