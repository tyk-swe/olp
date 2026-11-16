package cloudidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const storedLimit = 16384

func (s *Source) AWSCredentials(ctx context.Context, key Key, credential Credential, region string) (aws.Credentials, error) {
	if credential.stored {
		return staticAWSCredentials(ctx, credential.secret)
	}
	return s.chainedAWSCredentials(ctx, key, region)
}

func (s *Source) AWSWorkloadCredentials(ctx context.Context, key Key, region string) (aws.Credentials, error) {
	exchange := sts.New(sts.Options{Region: region, HTTPClient: s.metadata, RetryMaxAttempts: 1})
	return s.chainedAWSCredentials(ctx, key, region,
		awsconfig.WithWebIdentityRoleCredentialOptions(func(o *stscreds.WebIdentityRoleOptions) { o.Client = exchange }))
}

func staticAWSCredentials(ctx context.Context, secret []byte) (aws.Credentials, error) {
	var key struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SessionToken    string `json:"session_token"`
	}
	d := json.NewDecoder(bytes.NewReader(secret))
	d.DisallowUnknownFields()
	if len(secret) > storedLimit || d.Decode(&key) != nil || d.Decode(new(any)) != io.EOF ||
		!printable(key.AccessKeyID, 16, 256) || !printable(key.SecretAccessKey, 16, 1024) ||
		key.SessionToken != "" && !printable(key.SessionToken, 1, 8192) {
		return aws.Credentials{}, ErrRejected
	}
	resolved, err := credentials.NewStaticCredentialsProvider(key.AccessKeyID, key.SecretAccessKey, key.SessionToken).Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, ErrRejected
	}
	return resolved, nil
}

func (s *Source) chainedAWSCredentials(ctx context.Context, key Key, region string, chain ...func(*awsconfig.LoadOptions) error) (aws.Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	provider, ok := s.awsProviders.get(key)
	if !ok {
		options := append([]func(*awsconfig.LoadOptions) error{
			awsconfig.WithRegion(region), awsconfig.WithHTTPClient(s.metadata), awsconfig.WithRetryMaxAttempts(1),
			awsconfig.WithProcessCredentialOptions(func(o *processcreds.Options) { o.Timeout = Timeout }),
		}, chain...)
		loaded, err := awsconfig.LoadDefaultConfig(ctx, options...)
		if err != nil {
			return aws.Credentials{}, ErrRejected
		}
		provider = loaded.Credentials
		s.awsProviders.put(key, provider)
	}
	resolved, err := provider.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, exchangeFailure(err)
	}
	return resolved, nil
}
