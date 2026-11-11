package source

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
)

var (
	awsAccount = regexp.MustCompile(`^[0-9]{12}$`)
	awsRegion  = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-[0-9]+$`)
)

type awsScope struct {
	PayerAccountID   string   `json:"payer_account_id"`
	Bucket           string   `json:"bucket"`
	ReportPrefix     string   `json:"report_prefix"`
	Region           string   `json:"region"`
	LinkedAccountIDs []string `json:"linked_account_ids,omitempty"`
}

func parseAWSScope(raw json.RawMessage) (awsScope, error) {
	var scope awsScope
	if err := decodeStrict(raw, &scope); errors.Is(err, errTrailingData) {
		return awsScope{}, access.Invalid("scope", "scope must be a single JSON document")
	} else if err != nil {
		return awsScope{}, access.Invalid("scope", "AWS scope must be a JSON object with only AWS keys")
	}
	switch {
	case !awsAccount.MatchString(scope.PayerAccountID):
		return awsScope{}, access.Invalid("scope.payer_account_id", "payer account ID must contain 12 digits")
	case !connectors.ValidS3Bucket(scope.Bucket):
		return awsScope{}, access.Invalid("scope.bucket", "bucket must be a valid S3 bucket name")
	case len(scope.ReportPrefix) < 1 || len(scope.ReportPrefix) > 1024 || strings.HasPrefix(scope.ReportPrefix, "/") || strings.HasSuffix(scope.ReportPrefix, "/") || strings.Contains(scope.ReportPrefix, "..") || strings.ContainsAny(scope.ReportPrefix, "\r\n\x00"):
		return awsScope{}, access.Invalid("scope.report_prefix", "report prefix must name the CUR report")
	case !awsRegion.MatchString(scope.Region) || len(scope.Region) > 64:
		return awsScope{}, access.Invalid("scope.region", "region must be an AWS region")
	case len(scope.LinkedAccountIDs) > 100:
		return awsScope{}, access.Invalid("scope.linked_account_ids", "at most 100 linked accounts")
	case slices.ContainsFunc(scope.LinkedAccountIDs, func(id string) bool { return !awsAccount.MatchString(id) }):
		return awsScope{}, access.Invalid("scope.linked_account_ids", "linked account IDs must contain 12 digits")
	}
	return awsScope{PayerAccountID: scope.PayerAccountID, Bucket: scope.Bucket, ReportPrefix: scope.ReportPrefix, Region: scope.Region, LinkedAccountIDs: slices.Sorted(slices.Values(scope.LinkedAccountIDs))}, nil
}

type awsCredential interface {
	provider(ctx context.Context, req Cloud, region string) (aws.CredentialsProvider, error)
}

type awsStatic struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

type awsWorkload struct{}

func (c awsStatic) provider(context.Context, Cloud, string) (aws.CredentialsProvider, error) {
	return credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken), nil
}

func (awsWorkload) provider(ctx context.Context, req Cloud, region string) (aws.CredentialsProvider, error) {
	value, err := req.Auth.AWSWorkloadCredentials(ctx, region)
	if err != nil {
		return nil, fail(CodeCredential, err)
	}
	return credentials.NewStaticCredentialsProvider(value.AccessKeyID, value.SecretAccessKey, value.SessionToken), nil
}

type awsCredentialDocument struct {
	AuthMode        string `json:"auth_mode"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
}

func (c awsStatic) MarshalJSON() ([]byte, error) {
	return json.Marshal(awsCredentialDocument{AuthMode: "static", AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken})
}

func (awsWorkload) MarshalJSON() ([]byte, error) {
	return json.Marshal(awsCredentialDocument{AuthMode: "workload"})
}

func parseAWSCredential(text string) (awsCredential, error) {
	if len(text) > 8192 {
		return nil, access.Invalid("credential", "AWS credential exceeds 8192 bytes")
	}
	var doc awsCredentialDocument
	if err := decodeStrict(json.RawMessage(text), &doc); errors.Is(err, errTrailingData) {
		return nil, access.Invalid("credential", "AWS credential must be one document")
	} else if err != nil {
		return nil, access.Invalid("credential", "AWS credential must be a valid credential document")
	}
	keys := doc.AccessKeyID + doc.SecretAccessKey + doc.SessionToken
	switch doc.AuthMode {
	case "static":
		if len(doc.AccessKeyID) < 1 || len(doc.AccessKeyID) > 128 || len(doc.SecretAccessKey) < 1 || len(doc.SecretAccessKey) > 4096 || len(doc.SessionToken) > 4096 || strings.ContainsAny(keys, "\r\n\x00") {
			return nil, access.Invalid("credential", "AWS static credentials require an access key ID and secret access key")
		}
		return awsStatic{AccessKeyID: doc.AccessKeyID, SecretAccessKey: doc.SecretAccessKey, SessionToken: doc.SessionToken}, nil
	case "workload":
		if keys != "" {
			return nil, access.Invalid("credential", "AWS workload credentials cannot contain static keys")
		}
		return awsWorkload{}, nil
	default:
		return nil, access.Invalid("credential", "AWS auth_mode must be static or workload")
	}
}
