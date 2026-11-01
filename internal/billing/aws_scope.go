package billing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
)

type awsScope struct {
	PayerAccountID   string   `json:"payer_account_id"`
	Bucket           string   `json:"bucket"`
	ReportPrefix     string   `json:"report_prefix"`
	Region           string   `json:"region"`
	LinkedAccountIDs []string `json:"linked_account_ids,omitempty"`
}

var awsAccountPattern = regexp.MustCompile(`^[0-9]{12}$`)
var awsBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-[0-9]+$`)

func parseAWSScope(raw json.RawMessage) (awsScope, error) {
	var scope awsScope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scope); err != nil {
		return awsScope{}, access.Invalid("scope", "AWS scope must be a JSON object with only AWS keys")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return awsScope{}, access.Invalid("scope", "scope must be a single JSON document")
	}
	if !awsAccountPattern.MatchString(scope.PayerAccountID) {
		return awsScope{}, access.Invalid("scope.payer_account_id", "payer account ID must contain 12 digits")
	}
	if !awsBucketPattern.MatchString(scope.Bucket) || strings.Contains(scope.Bucket, "..") {
		return awsScope{}, access.Invalid("scope.bucket", "bucket must be a valid S3 bucket name")
	}
	if _, err := netip.ParseAddr(scope.Bucket); err == nil {
		return awsScope{}, access.Invalid("scope.bucket", "bucket must be a valid S3 bucket name")
	}
	if len(scope.ReportPrefix) < 1 || len(scope.ReportPrefix) > 1024 || strings.HasPrefix(scope.ReportPrefix, "/") || strings.HasSuffix(scope.ReportPrefix, "/") || strings.Contains(scope.ReportPrefix, "..") || strings.ContainsAny(scope.ReportPrefix, "\r\n\x00") {
		return awsScope{}, access.Invalid("scope.report_prefix", "report prefix must name the CUR report")
	}
	if !awsRegionPattern.MatchString(scope.Region) || len(scope.Region) > 64 {
		return awsScope{}, access.Invalid("scope.region", "region must be an AWS region")
	}
	if len(scope.LinkedAccountIDs) > 100 {
		return awsScope{}, access.Invalid("scope.linked_account_ids", "at most 100 linked accounts")
	}
	for _, id := range scope.LinkedAccountIDs {
		if !awsAccountPattern.MatchString(id) {
			return awsScope{}, access.Invalid("scope.linked_account_ids", "linked account IDs must contain 12 digits")
		}
	}
	return awsScope{PayerAccountID: scope.PayerAccountID, Bucket: scope.Bucket, ReportPrefix: scope.ReportPrefix, Region: scope.Region, LinkedAccountIDs: slices.Sorted(slices.Values(scope.LinkedAccountIDs))}, nil
}

func validateAWSScope(raw json.RawMessage) (json.RawMessage, error) {
	scope, err := parseAWSScope(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(scope)
}
