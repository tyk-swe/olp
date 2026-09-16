package connectors

import (
	"context"

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
