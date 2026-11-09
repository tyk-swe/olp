package vendor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

func (x exchange) bearer(ctx context.Context, body any) ([]byte, error) {
	return x.keyed(ctx, body, "Authorization", "Bearer ")
}

func (x exchange) azure(ctx context.Context, body any) ([]byte, error) {
	return x.keyed(ctx, body, "Ocp-Apim-Subscription-Key", "")
}

func (x exchange) path(suffix string) exchange {
	return x.at(strings.TrimRight(x.endpoint, "/") + suffix)
}

type bedrockConfig struct {
	Connection
	Region    string `json:"region"`
	Guardrail string `json:"guardrail"`
	Version   string `json:"version"`
}

func (c bedrockConfig) check() error {
	if c.Region == "" || !identifier.MatchString(c.Guardrail) || !identifier.MatchString(c.Version) {
		return errors.New("Bedrock requires region, guardrail and version")
	}
	return nil
}

func bedrock(ctx context.Context, x exchange, c bedrockConfig, r Request) (Decision, error) {
	if x.credential.AccessKeyID == "" || x.credential.SecretAccessKey == "" {
		return Decision{}, errUnavailable
	}
	credentials := aws.Credentials{AccessKeyID: x.credential.AccessKeyID, SecretAccessKey: x.credential.SecretAccessKey, SessionToken: x.credential.SessionToken}
	body := map[string]any{"source": pick(r, "INPUT", "OUTPUT"), "content": []any{map[string]any{"text": map[string]string{"text": r.Text}}}}
	raw, err := x.path("/guardrail/"+url.PathEscape(c.Guardrail)+"/version/"+url.PathEscape(c.Version)+"/apply").post(ctx, body, func(req *http.Request, encoded []byte) error {
		hash := sha256.Sum256(encoded)
		return v4.NewSigner().SignHTTP(ctx, credentials, req, hex.EncodeToString(hash[:]), "bedrock", c.Region, time.Now())
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action  string `json:"action"`
		Outputs []struct {
			Text string `json:"text"`
		} `json:"outputs"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Action {
	case "NONE":
		return Decision{}, nil
	case "GUARDRAIL_INTERVENED":
		if len(response.Outputs) != 1 || r.Text == "" {
			return flag("vendor.bedrock"), nil
		}
		return Decision{categories: []string{"vendor.bedrock"}, spans: []Span{{Start: 0, End: len(r.Text), Replacement: response.Outputs[0].Text}}}, nil
	default:
		return Decision{}, errUnavailable
	}
}

func promptShields(ctx context.Context, x exchange, _ Connection, r Request) (Decision, error) {
	body := map[string]any{"userPrompt": r.Text, "documents": []string{}}
	if r.Document {
		body = map[string]any{"documents": []string{r.Text}}
	}
	raw, err := x.path("/contentsafety/text:shieldPrompt?api-version=2024-09-01").azure(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		User *struct {
			Attack *bool `json:"attackDetected"`
		} `json:"userPromptAnalysis"`
		Documents []struct {
			Attack *bool `json:"attackDetected"`
		} `json:"documentsAnalysis"`
	}
	if json.Unmarshal(raw, &response) != nil || (!r.Document && (response.User == nil || response.User.Attack == nil)) || (r.Document && len(response.Documents) != 1) {
		return Decision{}, errUnavailable
	}
	categories := []string{}
	if response.User != nil && response.User.Attack != nil && *response.User.Attack {
		categories = append(categories, "vendor.prompt_attack")
	}
	for _, document := range response.Documents {
		if document.Attack == nil {
			return Decision{}, errUnavailable
		}
		if *document.Attack && !slices.Contains(categories, "vendor.document_attack") {
			categories = append(categories, "vendor.document_attack")
		}
	}
	return flag(categories...), nil
}

type contentSafetyConfig struct {
	Connection
	Threshold int `json:"threshold"`
}

func (c contentSafetyConfig) check() error {
	if c.Threshold < 0 || c.Threshold > 7 {
		return errors.New("invalid content safety threshold")
	}
	return nil
}

var contentSafetyCategories = []string{"Hate", "SelfHarm", "Sexual", "Violence"}

func contentSafety(ctx context.Context, x exchange, c contentSafetyConfig, r Request) (Decision, error) {
	raw, err := x.path("/contentsafety/text:analyze?api-version=2024-09-01").azure(ctx, map[string]any{"text": r.Text, "outputType": "EightSeverityLevels"})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Categories []struct {
			Category string `json:"category"`
			Severity *int   `json:"severity"`
		} `json:"categoriesAnalysis"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Categories) != len(contentSafetyCategories) {
		return Decision{}, errUnavailable
	}
	reported := make([]string, 0, len(response.Categories))
	exceeded := false
	for _, category := range response.Categories {
		if slices.Contains(reported, category.Category) || !slices.Contains(contentSafetyCategories, category.Category) || category.Severity == nil || *category.Severity < 0 || *category.Severity > 7 {
			return Decision{}, errUnavailable
		}
		reported = append(reported, category.Category)
		exceeded = exceeded || *category.Severity > c.Threshold
	}
	return flagIf(exceeded, "vendor.content_safety"), nil
}

type modelArmorConfig struct {
	Connection
	Template string `json:"template"`
}

func (c modelArmorConfig) check() error {
	if !strings.HasPrefix(c.Template, "projects/") || strings.Contains(c.Template, "..") {
		return errors.New("Model Armor requires a template resource")
	}
	return nil
}

func modelArmor(ctx context.Context, x exchange, c modelArmorConfig, r Request) (Decision, error) {
	field := pick(r, "userPromptData", "modelResponseData")
	raw, err := x.path("/v1/"+c.Template+":"+pick(r, "sanitizeUserPrompt", "sanitizeModelResponse")).bearer(ctx, map[string]any{field: map[string]string{"text": r.Text}})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Result *struct {
			State      string `json:"filterMatchState"`
			Invocation string `json:"invocationResult"`
		} `json:"sanitizationResult"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Result == nil || (response.Result.Invocation != "" && response.Result.Invocation != "SUCCESS") {
		return Decision{}, errUnavailable
	}
	switch response.Result.State {
	case "MATCH_FOUND":
		return flag("vendor.model_armor"), nil
	case "NO_MATCH_FOUND":
		return Decision{}, nil
	default:
		return Decision{}, errUnavailable
	}
}

func lakera(ctx context.Context, x exchange, _ Connection, r Request) (Decision, error) {
	raw, err := x.path("/v2/guard").bearer(ctx, map[string]any{"messages": messages(r)})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Flagged *bool `json:"flagged"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Flagged == nil {
		return Decision{}, errUnavailable
	}
	return flagIf(*response.Flagged, "vendor.lakera"), nil
}

type presidioConfig struct {
	Connection
	Language           string `json:"language"`
	AnonymizerEndpoint string `json:"anonymizer_endpoint"`
}

func (c presidioConfig) urls() []string {
	if c.AnonymizerEndpoint == "" {
		return c.Connection.urls()
	}
	return []string{c.Endpoint, c.AnonymizerEndpoint}
}

func (c presidioConfig) check() error {
	if c.AnonymizerEndpoint != "" && !validURL(c.AnonymizerEndpoint) {
		return errors.New("invalid Presidio anonymizer endpoint")
	}
	return nil
}

type presidioEntity struct {
	Type  string  `json:"entity_type"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Score float64 `json:"score"`
}

func presidio(ctx context.Context, x exchange, c presidioConfig, r Request) (Decision, error) {
	language := c.Language
	if language == "" {
		language = "en"
	}
	raw, err := x.path("/analyze").bearer(ctx, map[string]any{"text": r.Text, "language": language})
	if err != nil {
		return Decision{}, err
	}
	var entities []presidioEntity
	if json.Unmarshal(raw, &entities) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Decision{}, errUnavailable
	}
	if len(entities) == 0 {
		return Decision{}, nil
	}
	spans := make([]Span, 0, len(entities))
	for _, entity := range entities {
		start, end := runeOffset(r.Text, entity.Start), runeOffset(r.Text, entity.End)
		if start < 0 || end <= start || entity.Type == "" || entity.Score < 0 || entity.Score > 1 {
			return Decision{}, errUnavailable
		}
		spans = append(spans, Span{Start: start, End: end, Replacement: "[REDACTED]"})
	}
	if c.AnonymizerEndpoint == "" {
		return Decision{categories: []string{"vendor.presidio"}, spans: spans}, nil
	}
	return anonymize(ctx, x.at(strings.TrimRight(c.AnonymizerEndpoint, "/")+"/anonymize"), entities, r)
}

func anonymize(ctx context.Context, x exchange, entities []presidioEntity, r Request) (Decision, error) {
	raw, err := x.bearer(ctx, map[string]any{"text": r.Text, "analyzer_results": entities, "anonymizers": map[string]any{"DEFAULT": map[string]string{"type": "replace", "new_value": "[REDACTED]"}}})
	if err != nil {
		return Decision{}, err
	}
	var result struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Text == nil || !utf8.ValidString(*result.Text) {
		return Decision{}, errUnavailable
	}
	return Decision{categories: []string{"vendor.presidio"}, spans: []Span{{Start: 0, End: len(r.Text), Replacement: *result.Text}}}, nil
}

func runeOffset(text string, index int) int {
	if index < 0 {
		return -1
	}
	offset := 0
	for range index {
		if offset >= len(text) {
			return -1
		}
		_, n := utf8.DecodeRuneInString(text[offset:])
		offset += n
	}
	return offset
}
