package protocols

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Bedrock reranks through the Agent Runtime's Rerank API: one text query
// against inline text sources, scored by the reranking model the request names
// by ARN. AWS bills a query per hundred sources, so a rerank's usage is the
// queries its sources make.
//
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent-runtime_Rerank.html

// bedrockRerankSourcesPerQuery is how many sources AWS bills as one query.
const bedrockRerankSourcesPerQuery = 100

func encodeBedrockRerank(f Object, modelARN string) ([]byte, error) {
	var query string
	var documents []string
	if json.Unmarshal(f["query"], &query) != nil || json.Unmarshal(f["documents"], &documents) != nil {
		return nil, requestError("documents", "Bedrock reranks a text query against text documents.")
	}
	sources := make([]Object, len(documents))
	for i, document := range documents {
		sources[i] = Object{"type": raw("INLINE"), "inlineDocumentSource": raw(Object{"type": raw("TEXT"), "textDocument": raw(Object{"text": raw(document)})})}
	}
	configuration := Object{"modelConfiguration": raw(Object{"modelArn": raw(modelARN)})}
	if present(f["top_n"]) {
		configuration["numberOfResults"] = f["top_n"]
	}
	return json.Marshal(Object{
		"queries":                raw([]Object{{"type": raw("TEXT"), "textQuery": raw(Object{"text": raw(query)})}}),
		"sources":                raw(sources),
		"rerankingConfiguration": raw(Object{"type": raw("BEDROCK_RERANKING_MODEL"), "bedrockRerankingConfiguration": raw(configuration)}),
	})
}

// decodeBedrockRerank reads Bedrock's ranked sources as the OpenAI rerank
// result, with the documents the request sent where it asks for them.
func decodeBedrockRerank(body []byte, route string, request *openai.Request) (*openai.Completion, error) {
	f, err := object(body)
	if err != nil {
		return nil, protocolError("invalid rerank result")
	}
	if present(f["nextToken"]) {
		// Every result fits one response for the sources OLP sends.
		return nil, protocolError("Bedrock paginated a rerank result")
	}
	items := arr(f["results"])
	if len(items) == 0 {
		return nil, protocolError("missing rerank results")
	}
	documents := rerankDocuments(request)
	texts := rerankDocumentTexts(request)
	wantDocuments := rerankReturnDocuments(request)
	seen := map[int64]bool{}
	results := make([]Object, 0, len(items))
	for _, item := range items {
		result, err := object(item)
		if err != nil {
			return nil, protocolError("invalid rerank result")
		}
		index, ok := count(result["index"])
		if !ok || seen[index] || documents >= 0 && index >= int64(documents) {
			return nil, protocolError("invalid rerank index")
		}
		seen[index] = true
		var score float64
		if json.Unmarshal(result["relevanceScore"], &score) != nil || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return nil, protocolError("invalid rerank relevance score")
		}
		entry := Object{"index": raw(index), "relevance_score": raw(score)}
		if wantDocuments && index < int64(len(texts)) {
			entry["document"] = raw(texts[index])
		}
		results = append(results, entry)
	}
	queries := strconv.Itoa((max(documents, 1) + bedrockRerankSourcesPerQuery - 1) / bedrockRerankSourcesPerQuery)
	return &openai.Completion{FinishReason: "stop", Usage: &openai.Usage{MediaUnits: &queries},
		Body: raw(Object{"object": raw("list"), "model": raw(route), "results": raw(results)})}, nil
}
