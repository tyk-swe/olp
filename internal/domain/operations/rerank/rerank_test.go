package rerank

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

func rerankRequest(t *testing.T, dialectID, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	req, err := oif.NewRequest(oif.Descriptor{
		Operation: identity,
		Dialect:   oif.Identity{ID: dialectID, Revision: operations.Revision},
	}, doc)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func rerankResult(t *testing.T, dialectID, raw string) oif.Result {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	res, err := oif.NewResult(oif.Descriptor{
		Operation: identity,
		Dialect:   oif.Identity{ID: dialectID, Revision: operations.Revision},
	}, doc, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLiftRequestEnforcesDialectSpecificControlsAndDocuments(t *testing.T) {
	t.Parallel()
	valid := []struct {
		dialect string
		body    string
	}{
		{"rerank", `{"model":"rank-1","query":"cats","documents":["a",{"text":"b"}],"top_n":1,"return_documents":true}`},
		{"voyage-rerank", `{"model":"rank-1","query":"cats","documents":["a","b"],"top_k":2,"truncation":true}`},
		{"tei-rerank", `{"query":"cats","texts":["a","b"],"return_text":true,"raw_scores":true,"truncate":false}`},
		{"cohere-rerank-v2", `{"model":"rank-1","query":"cats","documents":["a","b"],"top_n":2,"max_tokens_per_doc":512,"priority":10}`},
	}
	for _, tc := range valid {
		req, err := liftRequest(rerankRequest(t, tc.dialect, tc.body), tc.dialect)
		if err != nil {
			t.Errorf("liftRequest(%s) rejected valid request: %v", tc.dialect, err)
		}
		if req.estimate < 1 {
			t.Errorf("liftRequest(%s) estimate = %d, want >= 1", tc.dialect, req.estimate)
		}
	}

	invalid := []struct {
		name    string
		dialect string
		body    string
	}{
		{"rerank rejects foreign top_k", "rerank", `{"model":"m","query":"q","documents":["a"],"top_k":1}`},
		{"voyage rejects colliding top_n", "voyage-rerank", `{"model":"m","query":"q","documents":["a"],"top_n":1}`},
		{"voyage rejects object document", "voyage-rerank", `{"model":"m","query":"q","documents":[{"text":"a"}]}`},
		{"tei rejects documents alias", "tei-rerank", `{"query":"q","documents":["a"]}`},
		{"tei rejects top_n", "tei-rerank", `{"query":"q","texts":["a"],"top_n":1}`},
		{"cohere v2 rejects foreign top_k", "cohere-rerank-v2", `{"model":"m","query":"q","documents":["a"],"top_k":1}`},
		{"cohere v2 rejects zero max_tokens_per_doc", "cohere-rerank-v2", `{"model":"m","query":"q","documents":["a"],"max_tokens_per_doc":0}`},
		{"cohere v2 rejects priority over 999", "cohere-rerank-v2", `{"model":"m","query":"q","documents":["a"],"priority":1000}`},
		{"empty documents rejected", "rerank", `{"model":"m","query":"q","documents":[]}`},
		{"non-positive top_n rejected", "rerank", `{"model":"m","query":"q","documents":["a"],"top_n":0}`},
		{"non-boolean return_documents rejected", "rerank", `{"model":"m","query":"q","documents":["a"],"return_documents":"yes"}`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := liftRequest(rerankRequest(t, tc.dialect, tc.body), tc.dialect); err == nil {
				t.Fatalf("liftRequest(%s) accepted invalid request: %s", tc.dialect, tc.name)
			}
		})
	}
}

func TestLiftResultEnforcesCandidateIdentityEchoAndUsage(t *testing.T) {
	t.Parallel()
	req := rerankRequest(t, "rerank", `{"model":"rank-1","query":"cats","documents":["first","second"],"top_n":2,"return_documents":true}`)
	okRes := rerankResult(t, "rerank", `{"results":[{"index":1,"relevance_score":0.9,"document":{"text":"second"}},{"index":0,"relevance_score":0.4,"document":"first"}],"usage":{"total_tokens":42}}`)
	lifted, err := liftResult(req, okRes, "rerank")
	if err != nil {
		t.Fatalf("liftResult rejected valid result: %v", err)
	}
	if lifted.usage == nil || lifted.usage.TotalTokens == nil || *lifted.usage.TotalTokens != 42 {
		t.Fatalf("usage = %+v, want TotalTokens=42", lifted.usage)
	}

	cohereReq := rerankRequest(t, "cohere-rerank-v2", `{"model":"rank-1","query":"cats","documents":["first"]}`)
	cohereRes := rerankResult(t, "cohere-rerank-v2", `{"results":[{"index":0,"relevance_score":0.8}],"meta":{"billed_units":{"search_units":1}}}`)
	cohereLifted, err := liftResult(cohereReq, cohereRes, "cohere-rerank-v2")
	if err != nil {
		t.Fatalf("cohere liftResult returned error: %v", err)
	}
	if cohereLifted.usage == nil || cohereLifted.usage.MediaUnits == nil || *cohereLifted.usage.MediaUnits != "1" {
		t.Fatalf("cohere usage = %+v, want MediaUnits=1", cohereLifted.usage)
	}

	violations := map[string]struct {
		dialect string
		req     string
		res     string
	}{
		"wrong candidate count": {
			"rerank",
			`{"model":"m","query":"q","documents":["a","b"],"top_n":2}`,
			`{"results":[{"index":0,"relevance_score":0.5}]}`,
		},
		"duplicate document index": {
			"rerank",
			`{"model":"m","query":"q","documents":["a","b"]}`,
			`{"results":[{"index":0,"relevance_score":0.9},{"index":0,"relevance_score":0.1}]}`,
		},
		"out-of-range index": {
			"voyage-rerank",
			`{"model":"m","query":"q","documents":["a"]}`,
			`{"data":[{"index":2,"relevance_score":0.9}]}`,
		},
		"missing echoed document when return_documents is true": {
			"rerank",
			`{"model":"m","query":"q","documents":["a"],"return_documents":true}`,
			`{"results":[{"index":0,"relevance_score":0.9}]}`,
		},
		"mutated echoed document bytes": {
			"tei-rerank",
			`{"query":"q","texts":["original"],"return_text":true}`,
			`[{"index":0,"score":0.9,"text":"tampered"}]`,
		},
		"negative total_tokens": {
			"rerank",
			`{"model":"m","query":"q","documents":["a"]}`,
			`{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":-5}}`,
		},
		"negative search_units": {
			"cohere-rerank-v2",
			`{"model":"m","query":"q","documents":["a"]}`,
			`{"results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":-1}}}`,
		},
	}

	for name, tc := range violations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := liftResult(rerankRequest(t, tc.dialect, tc.req), rerankResult(t, tc.dialect, tc.res), tc.dialect); err == nil {
				t.Fatalf("liftResult accepted invalid result: %s", name)
			}
		})
	}
}

func TestInputAndOutputTextEnforceFailClosedPolicyCoverage(t *testing.T) {
	t.Parallel()
	inTexts, err := inputText(rerankRequest(t, "rerank", `{"model":"m","query":"find","documents":["doc-a",{"title":"T","body":"B"}]}`), "rerank")
	if err != nil {
		t.Fatalf("inputText returned error: %v", err)
	}
	if len(inTexts) != 4 {
		t.Fatalf("inputText returned %d entries, want 4: %+v", len(inTexts), inTexts)
	}

	var incompatible *oif.Incompatibility
	if _, err := inputText(rerankRequest(t, "rerank", `{"model":"m","query":"find","documents":["a"],"custom_ext":"x"}`), "rerank"); !errors.As(err, &incompatible) || incompatible.Code != "policy_conflict" {
		t.Fatalf("unknown native control error = %v, want policy_conflict", err)
	}
	if _, err := inputText(rerankRequest(t, "rerank", `{"model":"m","query":"find","documents":[{"nested":{"a":"b"}}]}`), "rerank"); !errors.As(err, &incompatible) || incompatible.Code != "policy_conflict" {
		t.Fatalf("nested object document error = %v, want policy_conflict", err)
	}

	outTexts, err := outputText(rerankResult(t, "rerank", `{"results":[{"index":0,"relevance_score":0.9,"document":{"text":"hello"}}]}`))
	if err != nil || len(outTexts) != 1 || outTexts[0].Pointer != "/results/0/document/text" || outTexts[0].Value != "hello" {
		t.Fatalf("outputText = %+v err=%v", outTexts, err)
	}
	if _, err := outputText(rerankResult(t, "rerank", `{"results":[],"extra_field":"hidden"}`)); !errors.As(err, &incompatible) || incompatible.Code != "policy_conflict" {
		t.Fatalf("unknown output field error = %v, want policy_conflict", err)
	}
}
