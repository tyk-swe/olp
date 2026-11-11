package source_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const pollingKey = "sk-admin-xxxxxxxxxxxxxxxxxxxxxxxx"

func lookup(t *testing.T, kind source.Kind) source.Source {
	t.Helper()
	s, ok := source.Lookup(kind)
	if !ok {
		t.Fatalf("no source %s", kind)
	}
	return s
}

func TestScopesKeepTheCanonicalFormBehindStoredDigests(t *testing.T) {
	for _, tc := range []struct {
		kind  source.Kind
		input string
		want  string
	}{
		{contract.OpenaiCosts, `{"project_ids":["proj_zlegacy","proj_alegacy"]}`, `{"project_ids":["proj_zlegacy","proj_alegacy"]}`},
		{contract.OpenaiCosts, `null`, `{}`},
		{contract.AnthropicCostReport, `{"workspace_ids":["wrkspc_beta02","default"]}`, `{"workspace_ids":["wrkspc_beta02","default"]}`},
		{contract.AzureCostManagement, `{"container":"exports","storage_account":"billingaccount","export_name":"costs","billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","prefix":"/billing/","subscription_ids":["22222222-2222-2222-2222-222222222222","11111111-1111-1111-1111-111111111111","11111111-1111-1111-1111-111111111111"]}`, `{"billing_scope":"/providers/Microsoft.Billing/billingAccounts/1234567","export_name":"costs","storage_account":"billingaccount","container":"exports","prefix":"billing","subscription_ids":["11111111-1111-1111-1111-111111111111","22222222-2222-2222-2222-222222222222"]}`},
		{contract.AwsCur, `{"region":"us-east-1","report_prefix":"billing/cur","bucket":"billing-report-bucket","payer_account_id":"111122223333","linked_account_ids":["777788889999","444455556666"]}`, `{"payer_account_id":"111122223333","bucket":"billing-report-bucket","report_prefix":"billing/cur","region":"us-east-1","linked_account_ids":["444455556666","777788889999"]}`},
		{contract.GoogleBigquery, `{"table_id":"export","dataset_id":"billing_export","bigquery_project_id":"fixture-project","billing_account_id":"123456-ABCDEF-123456","project_ids":["zebra-project","alpha-project"]}`, `{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export","project_ids":["alpha-project","zebra-project"]}`},
	} {
		scope, err := lookup(t, tc.kind).Scope(json.RawMessage(tc.input))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.kind, tc.input, err)
		}
		again, err := lookup(t, tc.kind).Scope(scope.JSON())
		if string(scope.JSON()) != tc.want || err != nil || again.Digest() != scope.Digest() {
			t.Fatalf("%s: got %s then %s (%v), want %s both times", tc.kind, scope.JSON(), again.JSON(), err, tc.want)
		}
	}
}

func TestStoredCredentialsFromEarlierReleasesOpenOnlyForTheirFamily(t *testing.T) {
	googleStored := `{"source":"google_bigquery","auth_mode":"adc"}`
	for _, tc := range []struct {
		reader source.Kind
		stored string
		input  string
	}{
		{contract.OpenaiCosts, "{\n  \"source\": \"openai_costs\",\n  \"api_key\": \"" + pollingKey + "\"\n}", `"` + pollingKey + `"`},
		{contract.AnthropicCostReport, "{\n  \"source\": \"openai_costs\",\n  \"api_key\": \"" + pollingKey + "\"\n}", `"` + pollingKey + `"`},
		{contract.OpenaiCosts, pollingKey, `"` + pollingKey + `"`},
		{contract.AwsCur, "{\n  \"source\": \"aws_cur\",\n  \"auth_mode\": \"static\",\n  \"access_key_id\": \"BILLINGKEY1234567890\",\n  \"secret_access_key\": \"billing-secret-example\"\n}", `{"auth_mode":"static","access_key_id":"BILLINGKEY1234567890","secret_access_key":"billing-secret-example"}`},
		{contract.AzureCostManagement, "{\n  \"source\": \"azure_cost_management\",\n  \"credential\": {\n    \"auth_mode\": \"azure_client_secret\",\n    \"tenant_id\": \"tenant\",\n    \"client_id\": \"client\",\n    \"client_secret\": \"secret\"\n  }\n}", `{"auth_mode":"azure_client_secret","tenant_id":"tenant","client_id":"client","client_secret":"secret"}`},
		{contract.GoogleBigquery, googleStored, `"{\"auth_mode\":\"adc\"}"`},
		{contract.OpenaiCosts, googleStored, ""},
		{contract.AzureCostManagement, "{\n  \"source\": \"aws_cur\",\n  \"auth_mode\": \"workload\"\n}", ""},
		{contract.AwsCur, pollingKey, ""},
		{contract.GoogleBigquery, pollingKey, ""},
	} {
		reader := lookup(t, tc.reader)
		opened, err := reader.Stored([]byte(tc.stored))
		if tc.input == "" {
			if err == nil {
				t.Errorf("%s opened %s", tc.reader, tc.stored)
			}
			continue
		}
		fresh, freshErr := reader.Credential(json.RawMessage(tc.input))
		sealed, sealErr := fresh.Seal()
		resealed, resealErr := reader.Stored(sealed)
		if err != nil || freshErr != nil || sealErr != nil || resealErr != nil || opened != fresh || resealed != fresh {
			t.Errorf("%s reading %q: %v %v %v %v, want the same credential as a fresh %s", tc.reader, tc.stored, err, freshErr, sealErr, resealErr, tc.input)
		}
	}
}

func TestSourcesRejectInvalidScopesAndCredentials(t *testing.T) {
	for _, tc := range []struct {
		kind       source.Kind
		scope      string
		credential string
	}{
		{contract.OpenaiCosts, `{"workspace_ids":["default"]}`, `"short"`},
		{contract.OpenaiCosts, `{"project_ids":["proj_aaa111"],"unknown":1}`, `{"auth_mode":"adc"}`},
		{contract.OpenaiCosts, `{"project_ids":["proj_aaa111"]} {}`, `"has space in the key value"`},
		{contract.AnthropicCostReport, `{"workspace_ids":["not-a-workspace"]}`, `123`},
		{contract.AzureCostManagement, `{"billing_scope":"/bad","export_name":"costs","storage_account":"billingaccount","container":"exports"}`, `{"auth_mode":"azure_client_secret","tenant_id":"tenant"}`},
		{contract.AzureCostManagement, `{"billing_scope":"/subscriptions/11111111-1111-1111-1111-11111111111G","export_name":"costs","storage_account":"billingaccount","container":"exports"}`, `"` + pollingKey + `"`},
		{contract.AwsCur, `{"payer_account_id":"111122223333","bucket":"Bad_Bucket","report_prefix":"billing/cur","region":"us-east-1"}`, `{"auth_mode":"static","access_key_id":"BILLINGKEY1234567890"}`},
		{contract.AwsCur, `{"payer_account_id":"111122223333","bucket":"billing-report-bucket","report_prefix":"/billing","region":"us-east-1"}`, `{"auth_mode":"workload","access_key_id":"BILLINGKEY1234567890"}`},
		{contract.AwsCur, `{"payer_account_id":"11112222333","bucket":"billing-report-bucket","report_prefix":"billing/cur","region":"us-east-1"}`, `{"source":"aws_cur","auth_mode":"workload"}`},
		{contract.GoogleBigquery, `{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export","workspace_ids":["default"]}`, `{"auth_mode":"adc","service_account":{}}`},
		{contract.GoogleBigquery, `{"billing_account_id":"123456-ABCDEF-123456","bigquery_project_id":"fixture-project","dataset_id":"billing_export","table_id":"export","project_ids":["BAD"]}`, `{"auth_mode":"service_account","service_account":{"type":"service_account","private_key":"invalid","client_email":"billing@fixture-project.iam.gserviceaccount.com","token_uri":"https://oauth2.googleapis.com/token"}}`},
	} {
		if _, err := lookup(t, tc.kind).Scope(json.RawMessage(tc.scope)); err == nil {
			t.Errorf("%s accepted scope %s", tc.kind, tc.scope)
		}
		if _, err := lookup(t, tc.kind).Credential(json.RawMessage(tc.credential)); err == nil {
			t.Errorf("%s accepted credential %s", tc.kind, tc.credential)
		}
	}
}
