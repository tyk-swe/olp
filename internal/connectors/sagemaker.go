package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var sagemakerName = regexp.MustCompile(`^[a-zA-Z0-9](-*[a-zA-Z0-9])*$`)

func sagemakerModelValid(model string) bool {
	parts := strings.Split(model, "/")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if len(part) > 63 || !sagemakerName.MatchString(part) {
			return false
		}
	}
	return true
}

func sageMakerEndpoint(region string) (string, error) {
	return awsEndpoint("runtime.sagemaker", "SageMaker AI Runtime", region)
}

func PollyEndpoint(region string) (string, error) {
	return awsEndpoint("polly", "Amazon Polly", region)
}

func awsEndpoint(host, service, region string) (string, error) {
	bedrock, err := BedrockEndpoint(region, false)
	if err != nil {
		return "", err
	}
	domain, ok := strings.CutPrefix(bedrock, "https://bedrock-runtime."+region+".")
	if !ok || domain == "" || strings.Contains(domain, "/") {
		return "", fmt.Errorf("AWS region %s has no %s endpoint", region, service)
	}
	return "https://" + host + "." + region + "." + domain, nil
}

func sagemakerURL(base string, encodedWire wire.Format, model string) (string, error) {
	if encodedWire != wire.Chat {
		return "", errors.New("SageMaker endpoints serve the Chat Completions schema only")
	}
	endpoint, component, shared := strings.Cut(model, "/")
	path := "/endpoints/" + endpoint
	if shared {
		path += "/inference-components/" + component
	}
	return base + path + "/openai/v1/chat/completions", nil
}

func sagemakerBody(body []byte) []byte {
	return rewriteObject(body, func(fields map[string]json.RawMessage) {
		fields["model"] = json.RawMessage(`""`)
	})
}

func rewriteObject(body []byte, edit func(fields map[string]json.RawMessage)) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return body
	}
	edit(fields)
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

func (c Config) Rejection(status int, body []byte) (int, []byte) {
	if c.Kind != KindSageMaker || status != http.StatusFailedDependency {
		return status, body
	}
	var modelError struct {
		OriginalStatusCode int
		OriginalMessage    string
	}
	if json.Unmarshal(body, &modelError) != nil || modelError.OriginalStatusCode < 400 || modelError.OriginalStatusCode > 599 {
		return status, body
	}
	return modelError.OriginalStatusCode, []byte(modelError.OriginalMessage)
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

const sagemakerTokenLifetime = 15 * time.Minute

func sagemakerToken(ctx context.Context, creds aws.Credentials, region string, now time.Time) (string, error) {
	query := fmt.Sprintf("?Action=CallWithBearerToken&X-Amz-Expires=%d", int(sagemakerTokenLifetime/time.Second))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://sagemaker.amazonaws.com/"+query, nil)
	if err != nil {
		return "", err
	}
	// The presigned request has no body; botocore signs the empty body's hash.
	presigned, _, err := v4.NewSigner().PresignHTTP(ctx, creds, req, emptySHA256, "sagemaker", region, now)
	if err != nil {
		return "", err
	}
	return "sagemaker-api-key-" + base64.StdEncoding.EncodeToString([]byte(strings.TrimPrefix(presigned, "https://")+"&Version=1")), nil
}
