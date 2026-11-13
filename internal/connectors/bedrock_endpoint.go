package connectors

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

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

func (c Config) bedrockAgentRuntime(base string) (string, error) {
	runtime, err := BedrockEndpoint(c.CloudRegion, false)
	if err != nil || base != runtime {
		return "", errors.New("Bedrock rerank needs the region's default Bedrock Runtime endpoint")
	}
	return bedrockAgentRuntimeOrigin(runtime) + "/rerank", nil
}

func bedrockAgentRuntimeOrigin(runtime string) string {
	return strings.Replace(runtime, "://bedrock-runtime.", "://bedrock-agent-runtime.", 1)
}

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
