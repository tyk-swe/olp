package connectors

import (
	"context"
	"errors"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/tyk-swe/olp/internal/egress"
)

func (c Config) BedrockDuplexEndpoint() (string, error) {
	if err := c.ValidateProfile(); err != nil {
		return "", err
	}
	if c.Hosting() != "bedrock-nova-sonic" {
		return "", errors.New("provider profile does not support Bedrock bidirectional streaming")
	}
	if !cloudIdentifier.MatchString(c.CloudRegion) {
		return "", errors.New("Bedrock bidirectional streaming requires an AWS region")
	}
	if c.Endpoint != "" {
		return c.Endpoint, nil
	}
	return BedrockEndpoint(c.CloudRegion, false)
}

type BedrockStream struct {
	stream *bedrockruntime.InvokeModelWithBidirectionalStreamEventStream
	events <-chan types.InvokeModelWithBidirectionalStreamOutput
}

func (a *Auth) OpenBedrockDuplex(ctx context.Context, c Config, secret []byte, model string, client *http.Client) (*BedrockStream, egress.Sensitive, error) {
	var applied egress.Sensitive
	endpoint, err := c.BedrockDuplexEndpoint()
	if err != nil {
		return nil, applied, err
	}
	creds, err := a.awsCredentials(ctx, c, secret)
	if err != nil {
		return nil, applied, err
	}
	applied.Add(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	bound := c.Model(model)
	if bound == "" {
		return nil, applied, errors.New("Bedrock bidirectional streaming requires a bound model")
	}
	runtimeClient := bedrockruntime.New(bedrockruntime.Options{
		Region:       c.CloudRegion,
		Credentials:  aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)),
		BaseEndpoint: aws.String(endpoint),
		HTTPClient:   client,
		Retryer:      aws.NopRetryer{},
	})
	output, err := runtimeClient.InvokeModelWithBidirectionalStream(ctx, &bedrockruntime.InvokeModelWithBidirectionalStreamInput{ModelId: aws.String(bound)})
	if err != nil {
		return nil, applied, err
	}
	stream := output.GetStream()
	return &BedrockStream{stream: stream, events: stream.Events()}, applied, nil
}

func (s *BedrockStream) Send(ctx context.Context, payload []byte) error {
	return s.stream.Send(ctx, &types.InvokeModelWithBidirectionalStreamInputMemberChunk{
		Value: types.BidirectionalInputPayloadPart{Bytes: payload},
	})
}

func (s *BedrockStream) Receive(ctx context.Context) ([]byte, error) {
	select {
	case event, ok := <-s.events:
		if !ok {
			if err := s.stream.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("Bedrock bidirectional stream closed")
		}
		if chunk, isChunk := event.(*types.InvokeModelWithBidirectionalStreamOutputMemberChunk); isChunk {
			return chunk.Value.Bytes, nil
		}
		return nil, errors.New("Bedrock bidirectional stream sent an unknown event")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *BedrockStream) Close() error { return s.stream.Close() }
