package classification_test

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/classification"
	"strings"
	"testing"
)

func definition(t *testing.T, id string) operations.Dialect {
	t.Helper()
	for _, d := range classification.Definitions() {
		if d.Identity.ID == id {
			return d
		}
	}
	t.Fatal("missing dialect")
	return operations.Dialect{}
}
func request(t *testing.T, d operations.Dialect, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: d.Operation, Dialect: d.Identity}, doc)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func result(t *testing.T, r oif.Request, raw string) oif.Result {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := oif.NewResult(r.Descriptor(), doc, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a scoped refusal")
	}
	safe, ok := err.(interface {
		Incompatibility() (string, string, string, string)
	})
	if !ok {
		t.Fatalf("unscoped error: %v", err)
	}
	got, _, _, _ := safe.Incompatibility()
	if got != code {
		t.Fatalf("code %s, expected %s", got, code)
	}
}

func TestModerationPreservesNativeCategoriesThresholdsAndInputScope(t *testing.T) {
	d := definition(t, "openai-moderation")
	for _, tc := range []struct {
		name, input, kind string
		count             int
	}{
		{"scalar", `"text"`, "single-text", 1}, {"batch", `["first","second"]`, "text-batch", 2}, {"multimodal", `[{"type":"text","text":"text"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]`, "joint-multimodal", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"input":` + tc.input + `,"native_extension":{"threshold":-0,"future":false}}`
			r := request(t, d, raw)
			v, err := d.Request(r)
			if err != nil {
				t.Fatal(err)
			}
			scope := v.(classification.ModerationRequest).Scope()
			if scope.Kind != tc.kind || scope.Count != tc.count {
				t.Fatalf("scope %+v", scope)
			}
			row := `{"flagged":false,"categories":{"future/category":false,"__proto__":true},"category_scores":{"future/category":0.1000000000000000000001,"__proto__":1e-1000},"category_applied_input_types":{"future/category":["text","image"]},"thresholds":{"future/category":-0},"provider_notes":{"ordered":[false,0,"",null]}}`
			rows := row
			if tc.count == 2 {
				rows += "," + row
			}
			body := `{"id":"native-id","model":"native-model","results":[` + rows + `],"unknown":9007199254740993}`
			native := result(t, r, body)
			decoded, err := d.Result(r, native)
			if err != nil {
				t.Fatal(err)
			}
			out := decoded.(classification.ModerationResult)
			if out.Source().Source().Raw() != body || out.Scope() != scope || out.Decisions()[0].Scores.Members()[0].Value.Raw() != "0.1000000000000000000001" || out.Decisions()[0].Thresholds.Raw() != `{"future/category":-0}` {
				t.Fatal("native source, score or threshold scope changed")
			}
			detached := out.Decisions()
			detached[0].Position = 50
			if out.Decisions()[0].Position != 0 {
				t.Fatal("result view is mutable")
			}
			if v.(classification.ModerationRequest).Source().Document().Raw() != raw {
				t.Fatal("request changed")
			}
			if d.Usage != nil {
				t.Fatal("moderation categories are not usage")
			}
		})
	}
}
func TestModerationCorruptionDoesNotBecomeProviderSuccess(t *testing.T) {
	d := definition(t, "openai-moderation")
	r := request(t, d, `{"model":"route","input":["one","two"]}`)
	for _, raw := range []string{
		`{"id":"id","model":"m","results":[{"flagged":false,"categories":{"a":false},"category_scores":{"a":0}}]}`,
		`{"id":"id","model":"m","results":[{"flagged":null,"categories":{"a":false},"category_scores":{"a":0}},{"flagged":false,"categories":{"a":false},"category_scores":{"a":0}}]}`,
		`{"id":"id","model":"m","results":[{"flagged":false,"categories":{"a":false},"category_scores":{"b":0}},{"flagged":false,"categories":{"a":false},"category_scores":{"a":0}}]}`,
		`{"id":"id","model":"m","results":[{"flagged":false,"categories":{"a":false},"category_scores":{"a":"0"}},{"flagged":false,"categories":{"a":false},"category_scores":{"a":0}}]}`,
	} {
		_, err := d.Result(r, result(t, r, raw))
		requireCode(t, err, "fidelity_protocol_violation")
	}
	for _, raw := range []string{`{"input":null}`, `{"input":[]}`, `{"input":["text",{"type":"text","text":"part"}]}`, `{"input":[{"type":"image_url","image_url":null}]}`, `{"input":"text","model":null}`} {
		_, err := d.Request(request(t, d, raw))
		if err == nil {
			t.Fatalf("accepted corrupt input %s", raw)
		}
	}
}
func TestTEIPredictionRetainsPairBatchLabelsAndRawScoreUnits(t *testing.T) {
	d := definition(t, "tei-classification")
	for _, tc := range []struct {
		input string
		batch bool
		count int
		pair  bool
	}{
		{`"single"`, false, 1, false}, {`["single"]`, false, 1, false}, {`["first","second"]`, false, 1, true}, {`[["first"],["first","second"]]`, true, 2, false},
	} {
		r := request(t, d, `{"inputs":`+tc.input+`,"truncate":null,"raw_scores":true,"truncation_direction":"Left","unknown":{"threshold":-0}}`)
		v, err := d.Request(r)
		if err != nil {
			t.Fatal(err)
		}
		req := v.(classification.PredictRequest)
		if req.Batch() != tc.batch || len(req.Sequences()) != tc.count || req.Sequences()[0].Pair != tc.pair {
			t.Fatalf("changed TEI scope for %s", tc.input)
		}
		row := `[{"label":"same","score":2.5000000000000000001},{"label":"same","score":-7.25},{"label":"zero","score":-0,"native":false}]`
		body := row
		if tc.batch {
			body = "[" + row + "," + row + "]"
		}
		v, err = d.Result(r, result(t, r, body))
		if err != nil {
			t.Fatal(err)
		}
		out := v.(classification.PredictResult)
		predictions := out.Sets()[0].Predictions()
		if out.Source().Source().Raw() != body || predictions[0].Score.Raw() != "2.5000000000000000001" || predictions[1].Label.Raw() != `"same"` || predictions[2].Score.Raw() != "-0" || out.RawScores().Raw() != "true" {
			t.Fatal("native labels, duplicate labels, scores or provenance changed")
		}
		predictions[0].Position = 9
		if out.Sets()[0].Predictions()[0].Position != 0 {
			t.Fatal("mutable predictions")
		}
	}
	for _, raw := range []string{`{"inputs":[]}`, `{"inputs":["a","b","c"]}`, `{"inputs":[["a"],"b"]}`, `{"inputs":[["a","b","c"]]}`, `{"inputs":"a","raw_scores":null}`, `{"inputs":"a","truncation_direction":null}`, `{"inputs":"a","truncate":0}`} {
		_, err := d.Request(request(t, d, raw))
		if err == nil {
			t.Fatalf("accepted corrupt native shape %s", raw)
		}
	}
	pair := request(t, d, `{"inputs":["a","b"]}`)
	batch := request(t, d, `{"inputs":[["a"],["b"]]}`)
	for _, tc := range []struct {
		r    oif.Request
		body string
	}{{pair, `[[{"label":"a","score":0}]]`}, {batch, `[{"label":"a","score":0}]`}, {batch, `[[{"label":"a","score":0}]]`}, {pair, `[{"label":"a","score":null}]`}, {pair, `[{"label":0,"score":0}]`}} {
		_, err := d.Result(tc.r, result(t, tc.r, tc.body))
		requireCode(t, err, "fidelity_protocol_violation")
	}
}
func TestTEISimilarityPreservesSetOrderPresenceAndExactScores(t *testing.T) {
	d := definition(t, "tei-scoring")
	raw := `{"inputs":{"source_sentence":"same","sentences":["second","first","first"]},"parameters":{"truncate":false,"truncation_direction":"right","prompt_name":null}}`
	r := request(t, d, raw)
	v, err := d.Request(r)
	if err != nil {
		t.Fatal(err)
	}
	req := v.(classification.ScoringRequest)
	if req.Sentences().Raw() != `["second","first","first"]` || req.Parameters().Raw() != `{"truncate":false,"truncation_direction":"right","prompt_name":null}` {
		t.Fatal("native scope changed")
	}
	body := `[-2.5,1.000000000000000000001,1.000000000000000000001]`
	v, err = d.Result(r, result(t, r, body))
	if err != nil {
		t.Fatal(err)
	}
	out := v.(classification.ScoringResult)
	if out.Source().Source().Raw() != body || out.Scores()[1].InputIndex != 1 || out.Scores()[1].Score.Raw() != "1.000000000000000000001" {
		t.Fatal("similarity values normalized or reordered")
	}
	for _, body := range []string{`[0,1]`, `[0,1,2,3]`, `[0,null,2]`, `{"scores":[0,1,2]}`} {
		_, err := d.Result(r, result(t, r, body))
		requireCode(t, err, "fidelity_protocol_violation")
	}
	for _, params := range []string{"", `,"parameters":null`, `,"parameters":{"truncate":null,"prompt_name":""}`, `,"parameters":{}`} {
		if _, err := d.Request(request(t, d, `{"inputs":{"source_sentence":"","sentences":[""]}`+params+`}`)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestClassificationPolicyCoversTextAndRefusesOpaqueScopes(t *testing.T) {
	d := definition(t, "openai-moderation")
	r := request(t, d, `{"input":"covered"}`)
	texts, err := d.InputText(r)
	if err != nil || len(texts) != 1 || texts[0].Value != "covered" {
		t.Fatalf("text coverage: %v %v", texts, err)
	}
	for _, raw := range []string{`{"input":[{"type":"image_url","image_url":{"url":"https://example.test/image"}}]}`, `{"input":"text","native_extension":"opaque"}`} {
		_, err = d.InputText(request(t, d, raw))
		requireCode(t, err, "policy_conflict")
	}
	d = definition(t, "tei-scoring")
	_, err = d.InputText(request(t, d, `{"inputs":{"source_sentence":"x","sentences":["y"]},"parameters":{"prompt_name":"query"}}`))
	requireCode(t, err, "policy_conflict")
	d = definition(t, "tei-classification")
	texts, err = d.OutputText(result(t, request(t, d, `{"inputs":"text"}`), `[{"label":"native-label","score":-1,"metadata":{"note":"must-be-inspected"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	var visible []string
	for _, text := range texts {
		visible = append(visible, text.Value)
	}
	if !strings.Contains(strings.Join(visible, "|"), "must-be-inspected") {
		t.Fatal("unknown output text escaped policy inspection")
	}
}
func TestDefinitionsAreRegisteredAndModelOverlayDoesNotTouchNativeInput(t *testing.T) {
	registry := operations.NewRegistry()
	for _, d := range classification.Definitions() {
		if err := registry.Register(d); err != nil {
			t.Fatal(err)
		}
		r := request(t, d, string(d.Probe("route")))
		if _, err := d.Request(r); err != nil {
			t.Fatalf("%s probe: %v", d.Identity.ID, err)
		}
	}
	d := definition(t, "openai-moderation")
	r := request(t, d, `{"input":["a","b"],"extension":9007199254740993}`)
	changes, err := d.BindModel(r.Document(), "native-model")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := r.WithChanges(changes...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound.Document().Raw(), `"extension":9007199254740993`) || !strings.Contains(bound.Document().Raw(), `"input":["a","b"]`) {
		t.Fatal("model overlay modified native data")
	}
}
