package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestInspectInputTextDestinationWireFamilies(t *testing.T) {
	cases := []struct {
		family openai.Family
		body   string
		hits   int
	}{
		{openai.FamilyBedrock, `{"system":[{"text":"sys s3cr3t"}],"messages":[{"role":"user","content":[{"text":"say s3cr3t"},{"toolResult":{"toolUseId":"t","content":[{"text":"s3cr3t out"},{"json":{"k":"s3cr3t"}}]}}]},{"role":"assistant","content":[{"toolUse":{"toolUseId":"t","name":"f","input":{"q":"s3cr3t"}}}]}]}`, 5},
		{"bedrock_count", `{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"say s3cr3t"}]}]}}}`, 1},
		{openai.FamilyGeminiEmbeddings, `{"content":{"parts":[{"text":"s3cr3t"}]},"taskType":"RETRIEVAL_DOCUMENT","title":"s3cr3t title"}`, 2},
		{openai.FamilyGeminiEmbeddingsBatch, `{"requests":[{"model":"m","content":{"parts":[{"text":"s3cr3t"}]}},{"model":"m","content":{"parts":[{"text":"two s3cr3t"}]},"taskType":"RETRIEVAL_DOCUMENT","title":"s3cr3t title"}]}`, 3},
		{openai.FamilyVertexEmbeddings, `{"instances":[{"title":"s3cr3t","content":"s3cr3t body"}]}`, 2},
		{openai.FamilyBedrockEmbeddings, `{"inputText":"s3cr3t"}`, 1},
		{openai.FamilyBedrockRerank, `{"queries":[{"type":"TEXT","textQuery":{"text":"find s3cr3t"}}],"sources":[{"type":"INLINE","inlineDocumentSource":{"type":"TEXT","textDocument":{"text":"one s3cr3t"}}},{"type":"INLINE","inlineDocumentSource":{"type":"TEXT","textDocument":{"text":"two s3cr3t"}}}]}`, 3},
	}
	for _, tc := range cases {
		t.Run(string(tc.family), func(t *testing.T) {
			if !InputInspectable(tc.family) {
				t.Fatalf("InputInspectable(%s) = false", tc.family)
			}
			r := openai.NewEnvelope(tc.family, "route", false, decodeFields(t, tc.body))
			hits := 0
			out := inspectInputText(t, r, func(text string) (string, bool) {
				if strings.Contains(text, "s3cr3t") {
					hits++
				}
				return redactSecret(text)
			})
			if hits != tc.hits {
				t.Fatalf("matched %d texts, want %d", hits, tc.hits)
			}
			body, err := json.Marshal(out.Document())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "s3cr3t") || !strings.Contains(string(body), "[REDACTED]") {
				t.Fatalf("body was not redacted: %s", body)
			}
		})
	}
	if InputInspectable(openai.FamilyBedrockInvoke) {
		t.Fatal("bedrock_invoke must not claim input inspection coverage")
	}
}

func TestInspectInputTextBedrockRerankBlockStopsWalk(t *testing.T) {
	body, err := encodeBedrockRerank(Object{"query": raw("query"), "documents": raw([]string{"document", "later document"})}, rerankARN)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		blocked string
		hits    int
	}{{"query", 1}, {"document", 2}} {
		t.Run(tc.blocked, func(t *testing.T) {
			r := openai.NewEnvelope(openai.FamilyBedrockRerank, "route", false, decodeFields(t, string(body)))
			hits := 0
			found := false
			out := inspectInputText(t, r, func(text string) (string, bool) {
				hits++
				found = text == tc.blocked
				return text, found
			})
			if !found || hits != tc.hits {
				t.Fatalf("blocked=%v, inspected %d texts, want %d", found, hits, tc.hits)
			}
			if diff := sameJSON(t, out.OIF().Document().Bytes(), string(body)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func decodeFields(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
