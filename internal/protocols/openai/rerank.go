package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

var rerankKnown = map[string]bool{
	"model": true, "stream": true, "query": true, "documents": true,
	"top_n": true, "return_documents": true, "truncation": true,
}

func (r *Request) validateRerank() error {
	fields := r.fields
	for name := range fields {
		if !rerankKnown[name] {
			return invalid(name, "The rerank request supports model, query, documents, top_n, return_documents, and truncation only.")
		}
	}
	query, ok := stringField(fields, "query")
	if !ok || len(query) < 1 || len(query) > 100000 {
		return invalid("query", "query must be a string of 1 to 100,000 characters.")
	}
	raw, present := fields["documents"]
	items, ok := arrayField(fields, "documents")
	if !present || isNull(raw) || !ok || len(items) < 1 || len(items) > 1000 {
		return invalid("documents", "documents must be an array of 1 to 1000 strings.")
	}
	for i, item := range items {
		var document string
		if err := json.Unmarshal(item, &document); err != nil || document == "" || len(document) > 1<<20 {
			return invalid(fmt.Sprintf("documents[%d]", i), "Each document must be a non-empty string of at most 1 MiB.")
		}
	}
	if raw, present := fields["top_n"]; present && !isNull(raw) {
		n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
		if err != nil || n < 1 || n > int64(len(items)) {
			return invalid("top_n", "top_n must be an integer from 1 to the number of documents.")
		}
	}
	for _, name := range []string{"return_documents", "truncation"} {
		if raw, present := fields[name]; present && !isNull(raw) {
			var flag bool
			if err := json.Unmarshal(raw, &flag); err != nil {
				return invalid(name, name+" must be a boolean.")
			}
		}
	}
	return nil
}
