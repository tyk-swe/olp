package billing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
)

func TestS3BucketNamesAgreeAcrossBillingAndBatchStorage(t *testing.T) {
	for _, bucket := range []string{
		"billing-bucket", "a.b", strings.Repeat("a", 63),
		"ab", strings.Repeat("a", 64), "UPPERCASE", "-bucket", "a..b", "192.0.2.1",
	} {
		scope, err := json.Marshal(map[string]string{
			"payer_account_id": "123456789012", "bucket": bucket,
			"report_prefix": "report", "region": "us-east-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, scopeErr := ValidateScope(SourceAWS, scope)
		_, batchErr := connectors.NormalizeBatchStorage("bedrock", "bedrock-batch-inference", &connectors.BatchStorage{
			InputURIPrefix: "s3://" + bucket + "/input", OutputURIPrefix: "s3://" + bucket + "/output",
			RoleARN: "arn:aws:iam::123456789012:role/test",
		})
		if (scopeErr == nil) != (batchErr == nil) {
			t.Errorf("bucket %q, billing error %v, batch error %v", bucket, scopeErr, batchErr)
		}
	}
}

func TestSourceForKinds(t *testing.T) {
	for kind, want := range map[string]string{
		"openai":       SourceOpenAI,
		"anthropic":    SourceAnthropic,
		"azure_openai": SourceAzureCostManagement,
		"bedrock":      SourceAWS,
		"vertex_ai":    SourceGoogle,
	} {
		got, err := SourceFor(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if got != want {
			t.Fatalf("%s: got %q, want %q", kind, got, want)
		}
	}
	if _, err := SourceFor("unknown_kind"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestValidateScopeCanonical(t *testing.T) {
	for _, tc := range []struct {
		source string
		input  string
		want   string
	}{
		{SourceOpenAI, `{"project_ids":["proj_zlegacy","proj_alegacy"]}`, `{"project_ids":["proj_zlegacy","proj_alegacy"]}`},
		{SourceOpenAI, `{}`, `{}`},
		{SourceOpenAI, `null`, `{}`},
		{SourceOpenAI, ``, `{}`},
		{SourceAnthropic, `{"workspace_ids":["wrkspc_beta02","default"]}`, `{"workspace_ids":["wrkspc_beta02","default"]}`},
		{SourceAnthropic, `{}`, `{}`},
		{SourceAzureCostManagement, `{"container":"exports","storage_account":"billingaccount","export_name":"costs","billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","prefix":"/billing/","subscription_ids":["22222222-2222-2222-2222-222222222222","11111111-1111-1111-1111-111111111111","11111111-1111-1111-1111-111111111111"]}`, `{"billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","export_name":"costs","storage_account":"billingaccount","container":"exports","prefix":"billing","subscription_ids":["11111111-1111-1111-1111-111111111111","22222222-2222-2222-2222-222222222222"]}`},
		{SourceAzureCostManagement, `{"billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","export_name":"costs","storage_account":"billingaccount","container":"exports"}`, `{"billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","export_name":"costs","storage_account":"billingaccount","container":"exports"}`},
		{SourceAWS, `{"region":"us-east-1","report_prefix":"billing/cur","bucket":"billing-report-bucket","payer_account_id":"111122223333","linked_account_ids":["777788889999","444455556666"]}`, `{"payer_account_id":"111122223333","bucket":"billing-report-bucket","report_prefix":"billing/cur","region":"us-east-1","linked_account_ids":["444455556666","777788889999"]}`},
		{SourceGoogle, `{"table_id":"export","dataset_id":"billing_export","bigquery_project_id":"fixture-project","billing_account_id":"123456-ABCDEF-123456","project_ids":["zebra-project","alpha-project"]}`, `{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export","project_ids":["alpha-project","zebra-project"]}`},
	} {
		got, err := ValidateScope(tc.source, json.RawMessage(tc.input))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.source, tc.input, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.source, got, tc.want)
		}
		again, err := ValidateScope(tc.source, got)
		if err != nil {
			t.Fatalf("%s canonical revalidation: %v", tc.source, err)
		}
		if string(again) != tc.want {
			t.Fatalf("%s: canonical form not stable, got %s", tc.source, again)
		}
	}
}

func TestValidateScopeRejects(t *testing.T) {
	for _, tc := range []struct {
		source string
		input  string
	}{
		{"unknown_source", `{}`},
		{SourceOpenAI, `{"workspace_ids":["default"]}`},
		{SourceOpenAI, `{"project_ids":["not-a-project"]}`},
		{SourceOpenAI, `{"project_ids":["proj_aaa111"],"unknown":1}`},
		{SourceOpenAI, `{"project_ids":["proj_aaa111"]} {}`},
		{SourceAnthropic, `{"project_ids":["proj_aaa111"]}`},
		{SourceAnthropic, `{"workspace_ids":["not-a-workspace"]}`},
		{SourceAzureCostManagement, `{}`},
		{SourceAzureCostManagement, `{"billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","export_name":"costs","storage_account":"billingaccount","container":"exports","project_ids":["proj_aaa111"]}`},
		{SourceAzureCostManagement, `{"billing_scope":"/bad","export_name":"costs","storage_account":"billingaccount","container":"exports"}`},
		{SourceAWS, `{}`},
		{SourceAWS, `{"payer_account_id":"123","bucket":"billing-report-bucket","report_prefix":"billing/cur","region":"us-east-1"}`},
		{SourceAWS, `{"payer_account_id":"111122223333","bucket":"billing-report-bucket","report_prefix":"billing/cur","region":"us-east-1","unknown":1}`},
		{SourceGoogle, `{}`},
		{SourceGoogle, `{"billing_account_id":"bad","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export"}`},
		{SourceGoogle, `{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export","workspace_ids":["default"]}`},
	} {
		if _, err := ValidateScope(tc.source, json.RawMessage(tc.input)); err == nil {
			t.Fatalf("%s accepted %s", tc.source, tc.input)
		}
	}
}

func TestScopeDigestAgreesAcrossFormatting(t *testing.T) {
	messy, err := scopeDigest(SourceGoogle, json.RawMessage(`{"table_id":"export","dataset_id":"billing_export","bigquery_project_id":"fixture-project","billing_account_id":"123456-ABCDEF-123456","project_ids":["zebra-project","alpha-project"]}`))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := scopeDigest(SourceGoogle, json.RawMessage(`{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export","project_ids":["alpha-project","zebra-project"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if messy != canonical {
		t.Fatal("equal scopes produced different digests")
	}
	different, err := scopeDigest(SourceGoogle, json.RawMessage(`{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export"}`))
	if err != nil {
		t.Fatal(err)
	}
	if different == canonical {
		t.Fatal("different scopes produced the same digest")
	}
	if _, err := scopeDigest("unknown_source", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown source produced a digest")
	}
}
