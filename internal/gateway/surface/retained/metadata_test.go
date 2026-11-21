package retained

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestARetainedRequestKeepsOnlyModelAndBillingFactsAndRefusesControlsThatSpendOutsideItsContract(t *testing.T) {
	const judge, sampler, owner = "judge-model", "candidate-model", "key-1"
	capable := func(models ...string) []routing.Capability {
		var out []routing.Capability
		for _, model := range models {
			out = append(out, routing.Capability{Model: model, Operation: "eval", Surface: "openai", Mode: "unary"}, routing.Capability{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"})
		}
		return out
	}
	pin := &kernel.Pin{
		Model: judge,
		Slot:  routing.Slot{Enabled: true},
		Provider: routing.Provider{
			ID: "provider-1", Kind: "openai_compatible", Capabilities: capable(judge, sampler, "alias-model"),
			TuningDatasetURIPrefixes: []string{"gs://tuning/owned/"},
			Bindings:                 map[string]connectors.Binding{"alias-model": {Model: judge}},
		},
	}
	route := &routing.Route{Slug: "retained", Operations: []string{"generation", "eval"}, Targets: []routing.Target{
		{ProviderID: "provider-1", ProviderModel: judge}, {ProviderID: "provider-1", ProviderModel: sampler}, {ProviderID: "provider-1", ProviderModel: "alias-model"},
	}}
	graded := func(model string) string {
		return `{"data_source_config":{"type":"custom"},"testing_criteria":[{"type":"score_model","model":"` + model + `"}]}`
	}
	definition := &resources.Resource{Kind: "eval", Metadata: []byte(`{"authorized_models":["` + judge + `"],"serving_models":{"` + judge + `":"` + judge + `"}}`)}
	for _, tc := range []struct {
		name, kind, action, operation, body string
		parent                              *resources.Resource
		kept, refused                       string
	}{
		{name: "a vector store keeps only the provider model", kind: "vector_store", action: "create", body: `{"name":"docs","metadata":{"team":"a"}}`, kept: `{}`},
		{name: "metadata that is not an object", kind: "vector_store", action: "create", body: `{"metadata":"x"}`, refused: "metadata must be an object"},
		{name: "metadata naming the gateway's submission marker", kind: "vector_store", action: "create", body: `{"metadata":{"olp_submission_id":"x"}}`, refused: "reserved"},
		{name: "metadata naming the gateway's submission digest", kind: "vector_store", action: "create", body: `{"metadata":{"olp_submission_digest":"x"}}`, refused: "reserved"},

		{name: "a container keeps the default memory tier", kind: "container", action: "create", body: `{"name":"box"}`, kept: `{"memory_tier":"` + resources.DefaultRetainedContainerMemoryTier + `"}`},
		{name: "a container keeps the tier it asked for", kind: "container", action: "create", body: `{"memory_limit":"4g"}`, kept: `{"memory_tier":"4g"}`},
		{name: "a container tier the gateway cannot bill", kind: "container", action: "create", body: `{"memory_limit":"3g"}`, refused: "memory tier"},
		{name: "a container tier that is not text", kind: "container", action: "create", body: `{"memory_limit":4}`, refused: "memory tier"},
		{name: "container metadata", kind: "container", action: "create", body: `{"metadata":null}`, refused: "does not accept metadata"},
		{name: "a container file takes its container's tier", kind: "container_file", action: "create", body: `{}`, parent: &resources.Resource{Kind: "container", Metadata: []byte(`{"memory_tier":"16g"}`)}, kept: `{"memory_tier":"16g"}`},
		{name: "a container file whose container has no tier", kind: "container_file", action: "create", body: `{}`, parent: &resources.Resource{Kind: "container", Metadata: []byte(`{}`)}, refused: resources.ErrContract.Error()},
		{name: "a container file without its container", kind: "container_file", action: "create", body: `{}`, refused: resources.ErrContract.Error()},

		{name: "supervised tuning", kind: "fine_tuning_job", action: "create", body: `{"method":{"type":"supervised"}}`, kept: `{}`},
		{name: "preference tuning", kind: "fine_tuning_job", action: "create", body: `{"method":{"type":"dpo"}}`, kept: `{}`},
		{name: "tuning with no method", kind: "fine_tuning_job", action: "create", body: `{"method":null}`, kept: `{}`},
		{name: "a tuning method the gateway cannot settle", kind: "fine_tuning_job", action: "create", body: `{"method":{"type":"reinforcement"}}`, refused: "tuning method"},

		{name: "cloud tuning from an owned dataset", kind: "vertex_tuning_job", action: "create", body: `{"labels":{"team":"a"},"supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/owned/train.jsonl","validationDatasetUri":"gs://tuning/owned/check.jsonl"}}`, kept: `{}`},
		{name: "cloud tuning from a dataset outside the connection", kind: "vertex_tuning_job", action: "create", body: `{"supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/other/train.jsonl"}}`, refused: "training dataset"},
		{name: "cloud tuning without a dataset", kind: "vertex_tuning_job", action: "create", body: `{}`, refused: "training dataset"},
		{name: "cloud tuning validated outside the connection", kind: "vertex_tuning_job", action: "create", body: `{"supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/owned/train.jsonl","validationDatasetUri":"gs://elsewhere/check.jsonl"}}`, refused: "validation dataset"},
		{name: "cloud tuning labels that are not an object", kind: "vertex_tuning_job", action: "create", body: `{"labels":[],"supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/owned/train.jsonl"}}`, refused: resources.ErrContract.Error()},
		{name: "cloud tuning labels naming the submission marker", kind: "vertex_tuning_job", action: "create", body: `{"labels":{"olp_submission_id":"x"},"supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/owned/train.jsonl"}}`, refused: "reserved"},
		{name: "cloud tuning of custom weights", kind: "vertex_tuning_job", action: "create", body: `{"customBaseModel":"gs://weights","supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/owned/train.jsonl"}}`, refused: "only the route's base model"},
		{name: "cloud tuning by distillation", kind: "vertex_tuning_job", action: "create", body: `{"distillationSpec":{},"supervisedTuningSpec":{"trainingDatasetUri":"gs://tuning/owned/train.jsonl"}}`, refused: "only the route's base model"},

		{name: "a context cache keeps its serving model and that it holds no tools", kind: "context_cache", action: "create", body: `{"tools":[],"toolConfig":null}`, kept: `{"serving_model":"` + judge + `","no_cached_tools":true}`},
		{name: "a context cache holding tools", kind: "context_cache", action: "create", body: `{"tools":[{"functionDeclarations":[]}]}`, refused: "cached tools"},
		{name: "a context cache holding tool configuration", kind: "context_cache", action: "create", body: `{"tool_config":{}}`, refused: "cached tool configuration"},
		{name: "a context cache expiry change", kind: "context_cache", action: "patch", body: `{"ttl":"60s"}`, kept: `{"serving_model":"` + judge + `"}`},
		{name: "an empty context cache change", kind: "context_cache", action: "patch", body: `{}`, refused: "must not be empty"},
		{name: "a context cache change of anything but its expiry", kind: "context_cache", action: "patch", body: `{"ttl":"60s","displayName":"x"}`, refused: "only cache expiration"},

		{name: "an evaluation keeps the graders it may spend on", kind: "eval", action: "create", operation: "eval", body: graded(judge), kept: `{"authorized_models":["` + judge + `"],"serving_models":{"` + judge + `":"` + judge + `"}}`},
		{name: "an evaluation with no model graders keeps an empty list it can still run under", kind: "eval", action: "create", operation: "eval", body: `{"data_source_config":{"type":"custom"},"testing_criteria":[{"type":"string_check"}]}`, kept: `{"authorized_models":[],"serving_models":{}}`},
		{name: "an evaluation grading with a model off the route", kind: "eval", action: "create", operation: "eval", body: graded("stranger-model"), refused: "not authorized"},
		{name: "an evaluation grading with no model", kind: "eval", action: "create", operation: "eval", body: `{"testing_criteria":[{"type":"label_model"}]}`, refused: "explicitly authorized model"},
		{name: "an evaluation reading provider logs", kind: "eval", action: "create", operation: "eval", body: `{"data_source_config":{"type":"logs"}}`, refused: "owner-bound source"},
		{name: "an evaluation whose two names reach one provider model", kind: "eval", action: "create", operation: "eval", body: `{"testing_criteria":[{"type":"score_model","model":"` + judge + `"},{"type":"score_model","model":"alias-model"}]}`, refused: "distinct native models"},
		{name: "an evaluation rename keeps its graders", kind: "eval", action: "update", operation: "eval", body: `{"name":"renamed"}`, parent: definition, kept: `{"authorized_models":["` + judge + `"],"serving_models":{"` + judge + `":"` + judge + `"}}`},
		{name: "an evaluation change of anything but its name", kind: "eval", action: "update", operation: "eval", body: `{"testing_criteria":[]}`, parent: definition, refused: "only evaluation name and metadata"},
		{name: "a run adds its sampling model to the definition's graders", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"completions","model":"` + sampler + `","source":{"type":"file_content","content":[{"model":"ignored"}]}}}`, parent: definition, kept: `{"authorized_models":["` + sampler + `","` + judge + `"],"serving_models":{"` + judge + `":"` + judge + `","` + sampler + `":"` + sampler + `"}}`},
		{name: "a run without its definition", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"jsonl"}}`, refused: resources.ErrContract.Error()},
		{name: "a run under something that is not a definition", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"jsonl"}}`, parent: &resources.Resource{Kind: "container", Metadata: definition.Metadata}, refused: resources.ErrContract.Error()},
		{name: "a run of a definition stored without graders", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"jsonl"}}`, parent: &resources.Resource{Kind: "eval", Metadata: []byte(`{}`)}, refused: "recreate it"},
		{name: "a run of a definition bound to another provider model", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"jsonl"}}`, parent: &resources.Resource{Kind: "eval", Metadata: []byte(`{"authorized_models":["` + judge + `"],"serving_models":{"` + judge + `":"moved-model"}}`)}, refused: "compatible native grader"},
		{name: "a run of a definition whose grader left the route", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"completions","model":"` + sampler + `","source":{"type":"file_content","content":[]}}}`, parent: &resources.Resource{Kind: "eval", Metadata: []byte(`{"authorized_models":["retired-model"],"serving_models":{"retired-model":"retired-model"}}`)}, refused: resources.ErrContract.Error()},
		{name: "a run from a source the gateway cannot account", kind: "eval_run", action: "create", operation: "eval", body: `{"data_source":{"type":"stored_completions"}}`, parent: definition, refused: "unsupported evaluation data source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := oif.ParseJSON([]byte(tc.body), oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Kind: tc.kind, Operation: tc.operation}, Action: tc.action}
			fields, err := retainedInputMetadata(pin, route, owner, endpoint, source, tc.parent)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("refusal = %v, want one naming %q", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fields["upstream_model"] != judge || fields["contract_revision"] != retainedapi.Revision {
				t.Fatalf("kept %v, want the provider model and contract revision", fields)
			}
			delete(fields, "upstream_model")
			delete(fields, "contract_revision")
			got, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var want, have any
			if json.Unmarshal([]byte(tc.kept), &want) != nil || json.Unmarshal(got, &have) != nil || !reflect.DeepEqual(want, have) {
				t.Fatalf("kept %s beyond the provider model, want %s", got, tc.kept)
			}
		})
	}
}
