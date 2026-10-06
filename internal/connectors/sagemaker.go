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

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// SageMaker AI real-time endpoints serve the model their container hosts at an
// OpenAI-compatible path, routed by the endpoint, and by the inference
// component when several share it. OLP names an endpoint's model by the
// endpoint, and a component's as endpoint/component. The container must
// serve /v1/chat/completions and stream it as server-sent events, as the
// SageMaker vLLM and SGLang containers do.
//
// https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html

// KindSageMaker is the SageMaker AI real-time endpoint connector.
const KindSageMaker = "sagemaker"

// sagemakerName is the syntax of endpoint and inference component names.
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

// SageMakerEndpoint is the SageMaker AI Runtime endpoint of an AWS region, in
// the domain of the region's partition.
func SageMakerEndpoint(region string) (string, error) {
	return awsEndpoint("runtime.sagemaker", "SageMaker AI Runtime", region)
}

// PollyEndpoint is the Amazon Polly endpoint of an AWS region, in the domain
// of the region's partition.
func PollyEndpoint(region string) (string, error) {
	return awsEndpoint("polly", "Amazon Polly", region)
}

// awsEndpoint is the endpoint of an AWS service host in a region, in the
// domain the SDK's partition rules give the region's Bedrock Runtime.
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

func sagemakerURL(base string, wire openai.Family, model string) (string, error) {
	if wire != openai.FamilyChat {
		return "", errors.New("SageMaker endpoints serve the Chat Completions schema only")
	}
	endpoint, component, shared := strings.Cut(model, "/")
	path := "/endpoints/" + endpoint
	if shared {
		path += "/inference-components/" + component
	}
	return base + path + "/openai/v1/chat/completions", nil
}

// sagemakerBody clears the model a chat body names: the URL already selects
// it, and containers such as vLLM refuse a name they do not serve but accept
// an empty one, as AWS's own example sends.
func sagemakerBody(body []byte) []byte {
	return rewriteObject(body, func(fields map[string]json.RawMessage) {
		fields["model"] = json.RawMessage(`""`)
	})
}

// rewriteObject edits the members of a JSON object body; a body that is not
// an object is returned as it is.
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

// Rejection is the status and error body of an unsuccessful response as the
// model stated them. SageMaker reports a container's own rejection as a 424
// ModelError that carries the container's status and body, which classify
// the failure: a container's 400 is the caller's error, its 503 a server's.
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

// emptySHA256 is the hex SHA-256 of an empty body.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sagemakerTokenLifetime bounds a bearer token. OLP signs a fresh token for
// every request, so it need only outlive the request's dispatch.
const sagemakerTokenLifetime = 15 * time.Minute

// sagemakerToken is a SageMaker bearer token: a SigV4 presigned
// CallWithBearerToken request, signed locally with the provider's AWS
// credentials, as the SageMaker Python SDK's token generator builds it. The
// caller needs sagemaker:CallWithBearerToken and sagemaker:InvokeEndpoint.
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
