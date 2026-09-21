package protocols

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func encodeRerank(vendor string, f Object, model string) ([]byte, error) {
	delete(f, "stream")
	switch vendor {
	case "voyage":
		if value, ok := f["top_n"]; ok {
			f["top_k"] = value
			delete(f, "top_n")
		}
	case "cohere":
		if present(f["truncation"]) {
			return nil, requestError("truncation", "Cohere cannot represent truncation")
		}
	default:
		return nil, requestError("operation", "The selected provider does not support rerank")
	}
	return json.Marshal(f)
}

func rerankDocuments(r *openai.Request) int {
	if r == nil {
		return -1
	}
	return len(arr(r.Field("documents")))
}

func rerankReturnDocuments(r *openai.Request) bool {
	if r == nil {
		return false
	}
	var want bool
	_ = json.Unmarshal(r.Field("return_documents"), &want)
	return want
}

func decodeRerank(body []byte, route string, request *openai.Request) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid rerank result")
	}
	documents := rerankDocuments(request)
	wantDocuments := rerankReturnDocuments(request)
	c := &openai.Completion{FinishReason: "stop"}
	out := Object{"object": raw("list"), "model": raw(route)}
	if present(f["results"]) {
		results, err := rerankResults(arr(f["results"]), documents, wantDocuments)
		if err != nil {
			return nil, err
		}
		out["results"] = raw(results)
		if id, ok := f["id"]; ok && present(id) {
			if str(id) == "" {
				return nil, protocolError("invalid rerank id")
			}
			out["id"] = id
			c.UpstreamID = str(id)
		}
		if present(f["meta"]) {
			meta, e := optionalObject(f["meta"])
			if e != nil {
				return nil, e
			}
			if billed, e := optionalObject(meta["billed_units"]); e != nil {
				return nil, e
			} else if units, ok := decimalField(billed["search_units"]); ok {
				c.Usage = &openai.Usage{MediaUnits: &units}
			} else if present(billed["search_units"]) {
				return nil, protocolError("invalid rerank usage")
			}
		}
	} else if present(f["data"]) {
		results, err := rerankResults(arr(f["data"]), documents, wantDocuments)
		if err != nil {
			return nil, err
		}
		out["results"] = raw(results)
		if present(f["usage"]) {
			usage, e := optionalObject(f["usage"])
			if e != nil {
				return nil, e
			}
			n, ok := count(usage["total_tokens"])
			if !ok {
				return nil, protocolError("invalid rerank usage")
			}
			c.Usage = &openai.Usage{InputTokens: n, TotalTokens: n}
			out["usage"] = raw(Object{"total_tokens": raw(n)})
		}
	} else {
		return nil, protocolError("missing rerank results")
	}
	c.Body = raw(out)
	return c, nil
}

func rerankResults(items []json.RawMessage, documents int, wantDocuments bool) ([]Object, error) {
	if len(items) == 0 {
		return nil, protocolError("missing rerank results")
	}
	seen := map[int64]bool{}
	results := make([]Object, 0, len(items))
	for _, v := range items {
		item, e := object(v)
		if e != nil {
			return nil, protocolError("invalid rerank result")
		}
		index, ok := count(item["index"])
		if !ok || seen[index] || documents >= 0 && index >= int64(documents) {
			return nil, protocolError("invalid rerank index")
		}
		seen[index] = true
		var score float64
		if err := json.Unmarshal(item["relevance_score"], &score); err != nil || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return nil, protocolError("invalid rerank relevance score")
		}
		entry := Object{"index": raw(index), "relevance_score": raw(score)}
		if wantDocuments && present(item["document"]) {
			var document string
			if err := json.Unmarshal(item["document"], &document); err != nil {
				return nil, protocolError("invalid rerank document")
			}
			entry["document"] = raw(document)
		}
		results = append(results, entry)
	}
	return results, nil
}

func decimalField(raw json.RawMessage) (string, bool) {
	if !present(raw) {
		return "", false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		value, err := strconv.ParseFloat(number.String(), 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
			return number.String(), true
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		value, err := strconv.ParseFloat(text, 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
			return text, true
		}
	}
	return "", false
}
