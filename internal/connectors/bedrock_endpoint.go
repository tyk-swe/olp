package connectors

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// BedrockEndpoint uses the SDK's maintained partition rules without constructing
// an inference client, issuing a request, or enabling an SDK retry loop.
func BedrockEndpoint(region string, discovery bool) (string, error) {
	if discovery {
		endpoint, err := bedrock.NewDefaultEndpointResolverV2().ResolveEndpoint(context.Background(), bedrock.EndpointParameters{Region: aws.String(region)})
		if err != nil {
			return "", err
		}
		return endpoint.URI.String(), nil
	}
	endpoint, err := bedrockruntime.NewDefaultEndpointResolverV2().ResolveEndpoint(context.Background(), bedrockruntime.EndpointParameters{Region: aws.String(region)})
	if err != nil {
		return "", err
	}
	return endpoint.URI.String(), nil
}

// bedrockAgentRuntime is the Rerank address of the Bedrock Agent Runtime in
// the region a Bedrock Runtime endpoint serves. A custom endpoint, such as an
// interface VPC endpoint, names no Agent Runtime, so it cannot rerank.
func (c Config) bedrockAgentRuntime(base string) (string, error) {
	runtime, err := BedrockEndpoint(c.CloudRegion, false)
	if err != nil || base != runtime {
		return "", errors.New("Bedrock rerank needs the region's default Bedrock Runtime endpoint")
	}
	return strings.Replace(runtime, "://bedrock-runtime.", "://bedrock-agent-runtime.", 1) + "/rerank", nil
}

// BedrockModelARN is the ARN of a Bedrock foundation model in the
// connector's region and partition; an ARN names itself.
func (c Config) BedrockModelARN(model string) string {
	if strings.HasPrefix(model, "arn:") {
		return model
	}
	partition := "aws"
	switch {
	case strings.HasPrefix(c.CloudRegion, "cn-"):
		partition = "aws-cn"
	case strings.HasPrefix(c.CloudRegion, "us-gov-"):
		partition = "aws-us-gov"
	}
	return "arn:" + partition + ":bedrock:" + c.CloudRegion + "::foundation-model/" + model
}
