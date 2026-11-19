package cloudbatches

import (
	"regexp"
	"strconv"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
)

var displayName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type request struct {
	InputURI        string
	OutputURIPrefix string
	DisplayName     string
	TimeoutHours    int
}

func validate(raw []byte, adapter connectors.BatchAdapter, storage *connectors.BatchStorage) (request, *fault.Error) {
	doc, err := ingress.ParseObjectRequest(raw, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || doc.Root().Kind() != oif.Object {
		return request{}, fault.InvalidRequest("invalid_json", "The batch-job body must be one JSON object.", nil)
	}
	allowed := map[string]bool{"input_uri": true, "output_uri_prefix": true, "display_name": true, "timeout_hours": true}
	for _, field := range doc.Root().Members() {
		if !allowed[field.Name] {
			return request{}, fault.InvalidRequest("unsupported_parameter", "This batch-job parameter is not supported.", new(field.Name))
		}
	}
	var input request
	var present bool
	if value, ok := doc.Root().Lookup("input_uri"); ok {
		input.InputURI, present = value.Text()
	}
	if !present || input.InputURI == "" {
		return request{}, fault.InvalidRequest("missing_required_parameter", "input_uri is required.", new("input_uri"))
	}
	if value, ok := doc.Root().Lookup("output_uri_prefix"); ok {
		input.OutputURIPrefix, present = value.Text()
	}
	if !present || input.OutputURIPrefix == "" {
		return request{}, fault.InvalidRequest("missing_required_parameter", "output_uri_prefix is required.", new("output_uri_prefix"))
	}
	if storage == nil || !storage.Contains(storage.InputURIPrefix, input.InputURI) ||
		!storage.ContainsPrefix(storage.OutputURIPrefix, input.OutputURIPrefix) {
		return request{}, fault.InvalidRequest("storage_scope", "The input or output URI is outside the configured batch storage prefixes.", nil)
	}
	if value, ok := doc.Root().Lookup("display_name"); ok {
		input.DisplayName, present = value.Text()
		if !present || !displayName.MatchString(input.DisplayName) {
			return request{}, fault.InvalidRequest("invalid_value", "display_name must use 1–128 letters, digits, underscores, or hyphens.", new("display_name"))
		}
	}
	if value, ok := doc.Root().Lookup("timeout_hours"); ok {
		if adapter != connectors.BatchBedrock {
			return request{}, fault.InvalidRequest("unsupported_parameter", "timeout_hours is only supported by Bedrock batch inference.", new("timeout_hours"))
		}
		if value.Kind() != oif.Number {
			return request{}, fault.InvalidRequest("invalid_value", "timeout_hours must be an integer from 24 to 168.", new("timeout_hours"))
		}
		parsed, parseErr := strconv.Atoi(value.Raw())
		if parseErr != nil || parsed < 24 || parsed > 168 {
			return request{}, fault.InvalidRequest("invalid_value", "timeout_hours must be an integer from 24 to 168.", new("timeout_hours"))
		}
		input.TimeoutHours = parsed
	}
	return input, nil
}
