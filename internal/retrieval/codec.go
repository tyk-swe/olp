// Package retrieval implements query-only access to operator-managed indexes.
// A binding grants access to one backend resource; requests never name indexes
// or provider endpoints and cannot create or synchronize them.
package retrieval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

const (
	Bedrock = "bedrock_knowledge_base"
	Vertex  = "vertex_rag"
	Azure   = "azure_ai_search"
)

type Fields struct {
	Text     string            `json:"text,omitempty"`
	ID       string            `json:"id,omitempty"`
	Title    string            `json:"title,omitempty"`
	URL      string            `json:"url,omitempty"`
	Search   []string          `json:"search,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Filter struct {
	Type    string          `json:"type"`
	Key     string          `json:"key,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Filters []Filter        `json:"filters,omitempty"`
}
type Query struct {
	Query                     string       `json:"query"`
	MaxResults                *int         `json:"max_num_results,omitempty"`
	Filters                   *Filter      `json:"filters,omitempty"`
	VectorDistanceThreshold   *json.Number `json:"vector_distance_threshold,omitempty"`
	VectorSimilarityThreshold *json.Number `json:"vector_similarity_threshold,omitempty"`
}
type Result struct {
	Text       string         `json:"text"`
	Score      json.Number    `json:"score"`
	ScoreKind  string         `json:"score_kind"`
	Citation   any            `json:"citation"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(/[A-Za-z_][A-Za-z0-9_]*)*$`)
var kbID = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)
var indexName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
var corpusName = regexp.MustCompile(`^projects/[A-Za-z0-9_-]+/locations/[a-z0-9-]+/ragCorpora/[A-Za-z0-9_-]+$`)

func ResourceValid(backend, resource string) bool {
	switch backend {
	case Bedrock:
		return kbID.MatchString(resource)
	case Vertex:
		return corpusName.MatchString(resource)
	case Azure:
		return indexName.MatchString(resource)
	}
	return false
}
func ProviderKind(backend string) string {
	switch backend {
	case Bedrock:
		return "bedrock"
	case Vertex:
		return "vertex_ai"
	case Azure:
		return Azure
	}
	return ""
}
func ValidateFields(backend string, fields Fields) error {
	if len(fields.Metadata) > 64 || len(fields.Search) > 32 {
		return errors.New("too many field mappings")
	}
	for _, name := range append([]string{fields.Text, fields.ID, fields.Title, fields.URL}, fields.Search...) {
		if name != "" && !fieldName.MatchString(name) {
			return errors.New("invalid backend field mapping")
		}
	}
	for key, name := range fields.Metadata {
		if key == "" || len(key) > 128 || !fieldName.MatchString(name) {
			return errors.New("invalid metadata field mapping")
		}
	}
	if backend == Azure && (fields.Text == "" || fields.ID == "") {
		return errors.New("Azure search requires text and id field mappings")
	}
	if backend != Azure && (fields.Text != "" || fields.ID != "" || fields.Title != "" || fields.URL != "" || len(fields.Search) > 0) {
		return errors.New("this backend uses native text and citation fields")
	}
	return nil
}

func strictDecode(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("one JSON value required")
	}
	return nil
}

// Encode refuses every control that the chosen backend cannot represent.
func Encode(backend, resource string, fields Fields, raw []byte) ([]byte, error) {
	if doc, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 1 << 20}); err != nil || doc.Root().Kind() != oif.Object {
		return nil, errors.New("one unambiguous JSON query object required")
	}
	if !ResourceValid(backend, resource) {
		return nil, errors.New("invalid backend resource")
	}
	if err := ValidateFields(backend, fields); err != nil {
		return nil, err
	}
	var q Query
	if err := strictDecode(raw, &q); err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Query) == "" || len(q.Query) > 20000 {
		return nil, errors.New("query must contain 1–20000 bytes")
	}
	limit := 10
	if q.MaxResults != nil {
		limit = *q.MaxResults
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("max_num_results must be between 1 and 100")
	}
	thresholds := q.VectorDistanceThreshold != nil || q.VectorSimilarityThreshold != nil
	if thresholds {
		var native map[string]json.RawMessage
		_ = json.Unmarshal(raw, &native)
		for _, field := range []string{"vector_distance_threshold", "vector_similarity_threshold"} {
			if value := bytes.TrimSpace(native[field]); len(value) > 0 && (value[0] == '"' || bytes.Equal(value, []byte("null"))) {
				return nil, errors.New("vector thresholds must be JSON numbers")
			}
		}
	}
	if thresholds && (backend != Vertex || q.VectorDistanceThreshold != nil && q.VectorSimilarityThreshold != nil) {
		return nil, errors.New("vector thresholds require Vertex and exactly one threshold")
	}
	var body any
	switch backend {
	case Bedrock:
		config := map[string]any{"numberOfResults": limit}
		if q.Filters != nil {
			f, err := bedrockFilter(*q.Filters, fields, 0)
			if err != nil {
				return nil, err
			}
			config["filter"] = f
		}
		body = map[string]any{"retrievalQuery": map[string]any{"text": q.Query}, "retrievalConfiguration": map[string]any{"vectorSearchConfiguration": config}}
	case Vertex:
		if q.Filters != nil {
			return nil, errors.New("Vertex metadata filters are not supported by this binding; use a supported vector threshold")
		}
		config := map[string]any{"topK": limit}
		if q.VectorDistanceThreshold != nil {
			config["filter"] = map[string]any{"vectorDistanceThreshold": q.VectorDistanceThreshold}
		}
		if q.VectorSimilarityThreshold != nil {
			config["filter"] = map[string]any{"vectorSimilarityThreshold": q.VectorSimilarityThreshold}
		}
		body = map[string]any{"query": map[string]any{"text": q.Query, "ragRetrievalConfig": config}, "vertexRagStore": map[string]any{"ragResources": []any{map[string]any{"ragCorpus": resource}}}}
	case Azure:
		config := map[string]any{"search": q.Query, "top": limit}
		if len(fields.Search) > 0 {
			config["searchFields"] = strings.Join(fields.Search, ",")
		}
		selected := []string{fields.Text, fields.ID}
		for _, f := range []string{fields.Title, fields.URL} {
			if f != "" {
				selected = append(selected, f)
			}
		}
		for _, f := range fields.Metadata {
			selected = append(selected, f)
		}
		slices.Sort(selected)
		selected = slices.Compact(selected)
		config["select"] = strings.Join(selected, ",")
		if q.Filters != nil {
			f, err := azureFilter(*q.Filters, fields, 0)
			if err != nil {
				return nil, err
			}
			config["filter"] = f
		}
		body = config
	default:
		return nil, errors.New("unsupported retrieval backend")
	}
	return json.Marshal(body)
}

func filterParts(f Filter, fields Fields, depth int) (string, any, error) {
	if depth > 8 {
		return "", nil, errors.New("filter nesting exceeds eight levels")
	}
	if f.Type == "and" || f.Type == "or" {
		if f.Key != "" || len(f.Value) > 0 || len(f.Filters) < 2 || len(f.Filters) > 16 {
			return "", nil, errors.New("compound filters require 2–16 filters")
		}
		return "", nil, nil
	}
	name, ok := fields.Metadata[f.Key]
	if !ok || len(f.Filters) > 0 || !slices.Contains([]string{"eq", "ne", "gt", "gte", "lt", "lte", "in", "nin"}, f.Type) {
		return "", nil, errors.New("unsupported filter or unmapped metadata key")
	}
	var value any
	d := json.NewDecoder(bytes.NewReader(f.Value))
	d.UseNumber()
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF {
		return "", nil, errors.New("invalid filter value")
	}
	if f.Type == "in" || f.Type == "nin" {
		items, ok := value.([]any)
		if !ok || len(items) < 1 || len(items) > 100 {
			return "", nil, errors.New("membership filters require 1–100 values")
		}
		for _, v := range items {
			if !scalar(v) {
				return "", nil, errors.New("membership values must be scalar")
			}
		}
	} else if !scalar(value) {
		return "", nil, errors.New("filter value must be scalar")
	}
	if _, ok := value.(bool); ok && f.Type != "eq" && f.Type != "ne" {
		return "", nil, errors.New("boolean filters require equality")
	}
	return name, value, nil
}
func scalar(v any) bool {
	switch v.(type) {
	case string, json.Number, bool:
		return true
	}
	return false
}
func bedrockFilter(f Filter, fields Fields, depth int) (any, error) {
	name, value, err := filterParts(f, fields, depth)
	if err != nil {
		return nil, err
	}
	if f.Type == "and" || f.Type == "or" {
		children := []any{}
		for _, child := range f.Filters {
			v, err := bedrockFilter(child, fields, depth+1)
			if err != nil {
				return nil, err
			}
			children = append(children, v)
		}
		op := "andAll"
		if f.Type == "or" {
			op = "orAll"
		}
		return map[string]any{op: children}, nil
	}
	op := map[string]string{"eq": "equals", "ne": "notEquals", "gt": "greaterThan", "gte": "greaterThanOrEquals", "lt": "lessThan", "lte": "lessThanOrEquals", "in": "in", "nin": "notIn"}[f.Type]
	return map[string]any{op: map[string]any{"key": name, "value": value}}, nil
}
func odataLiteral(v any) string {
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return fmt.Sprint(v)
}
func azureFilter(f Filter, fields Fields, depth int) (string, error) {
	name, value, err := filterParts(f, fields, depth)
	if err != nil {
		return "", err
	}
	if f.Type == "and" || f.Type == "or" {
		parts := []string{}
		for _, child := range f.Filters {
			part, err := azureFilter(child, fields, depth+1)
			if err != nil {
				return "", err
			}
			parts = append(parts, part)
		}
		return "(" + strings.Join(parts, " "+f.Type+" ") + ")", nil
	}
	if f.Type == "in" || f.Type == "nin" {
		parts := []string{}
		op, join := "eq", " or "
		if f.Type == "nin" {
			op, join = "ne", " and "
		}
		for _, item := range value.([]any) {
			parts = append(parts, name+" "+op+" "+odataLiteral(item))
		}
		return "(" + strings.Join(parts, join) + ")", nil
	}
	op := map[string]string{"eq": "eq", "ne": "ne", "gt": "gt", "gte": "ge", "lt": "lt", "lte": "le"}[f.Type]
	return "(" + name + " " + op + " " + odataLiteral(value) + ")", nil
}

// Decode preserves native score meaning and citation objects. Scores from
// different backends are deliberately not normalized or made comparable.
func Decode(backend string, fields Fields, raw []byte) ([]Result, error) {
	if doc, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 32 << 20}); err != nil || doc.Root().Kind() != oif.Object {
		return nil, errors.New("invalid unambiguous retrieval response")
	}
	var envelope map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&envelope) != nil || envelope == nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid retrieval response")
	}
	var entries []any
	switch backend {
	case Bedrock:
		var ok bool
		entries, ok = envelope["retrievalResults"].([]any)
		if !ok {
			return nil, errors.New("missing retrieval results")
		}
	case Vertex:
		contexts, ok := envelope["contexts"].(map[string]any)
		if !ok {
			if _, present := envelope["contexts"]; present {
				return nil, errors.New("invalid Vertex contexts")
			}
			return []Result{}, nil
		}
		value, present := contexts["contexts"]
		if present {
			var ok bool
			entries, ok = value.([]any)
			if !ok {
				return nil, errors.New("invalid Vertex contexts")
			}
		}
	case Azure:
		var ok bool
		entries, ok = envelope["value"].([]any)
		if !ok {
			return nil, errors.New("missing search results")
		}
	default:
		return nil, errors.New("unsupported retrieval backend")
	}
	if entries == nil {
		entries = []any{}
	}
	if len(entries) > 1000 {
		return nil, errors.New("too many retrieval results")
	}
	results := make([]Result, 0, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("invalid retrieval result")
		}
		r := Result{ScoreKind: "score"}
		switch backend {
		case Bedrock:
			content, _ := item["content"].(map[string]any)
			r.Text, _ = content["text"].(string)
			r.Score, _ = item["score"].(json.Number)
			r.Citation = item["location"]
			r.Attributes, _ = item["metadata"].(map[string]any)
		case Vertex:
			r.Text, _ = item["text"].(string)
			r.Score, _ = item["score"].(json.Number)
			if v, ok := item["distance"].(json.Number); ok {
				r.Score, r.ScoreKind = v, "distance"
			}
			r.Citation = map[string]any{"source_uri": item["sourceUri"], "source_display_name": item["sourceDisplayName"], "source_name": item["sourceName"], "chunk": item["chunk"]}
		case Azure:
			r.Text, _ = fieldValue(item, fields.Text).(string)
			r.Score, _ = item["@search.score"].(json.Number)
			r.Citation = map[string]any{"id": fieldValue(item, fields.ID), "url": fieldValue(item, fields.URL), "title": fieldValue(item, fields.Title)}
			r.Attributes = map[string]any{}
			for key, name := range fields.Metadata {
				if v := fieldValue(item, name); v != nil {
					r.Attributes[key] = v
				}
			}
		}
		if r.Text == "" || r.Score == "" {
			return nil, errors.New("retrieval result omitted text or native score")
		}
		results = append(results, r)
	}
	return results, nil
}

func fieldValue(item map[string]any, path string) any {
	if path == "" {
		return nil
	}
	var value any = item
	for _, key := range strings.Split(path, "/") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}
