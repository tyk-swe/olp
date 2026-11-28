package openaistate

import (
	"bufio"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) validateBatchInput(part *staging.Part, model string, provider *routing.Provider, route *routing.Route) (string, int, error) {
	opened, err := s.spool.Open(part.Handle)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = opened.File.Close() }()
	scanner := bufio.NewScanner(opened.File)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	seen := map[string]struct{}{}
	endpoint := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || len(seen) >= 10000 {
			return "", 0, resources.ErrContract
		}
		doc, err := ingress.ParseObjectRequest(line, oif.Limits{MaxBytes: 1 << 20})
		if err != nil || doc.Root().Kind() != oif.Object {
			return "", 0, resources.ErrContract
		}
		get := func(parent oif.Value, name string) (string, bool) {
			field, ok := parent.Lookup(name)
			text, valid := field.Text()
			return text, ok && valid && text != ""
		}
		custom, ok := get(doc.Root(), "custom_id")
		if !ok || len(custom) > 128 {
			return "", 0, resources.ErrContract
		}
		if _, duplicate := seen[custom]; duplicate {
			return "", 0, resources.ErrContract
		}
		seen[custom] = struct{}{}
		method, ok := get(doc.Root(), "method")
		if !ok || method != http.MethodPost {
			return "", 0, resources.ErrContract
		}
		path, ok := get(doc.Root(), "url")
		classificationPath := provider.Connector().BatchLineEndpoint(path)
		if !ok || classificationPath != "/v1/chat/completions" && classificationPath != "/v1/responses" && classificationPath != "/v1/embeddings" || endpoint != "" && endpoint != path {
			return "", 0, resources.ErrContract
		}
		operation := "generation"
		if classificationPath == "/v1/embeddings" {
			operation = "embeddings"
		}
		profile, err := provider.Connector().Profile()
		if err != nil || !slices.Contains(route.Operations, operation) || !provider.Supports(model, operation, "openai", "unary") ||
			!provider.Connector().Supports(operation, "openai", "unary") ||
			classificationPath == "/v1/chat/completions" && profile.Dialect != "openai-chat" ||
			classificationPath == "/v1/responses" && profile.Dialect != "openai-responses" ||
			classificationPath == "/v1/embeddings" && profile.OperationDialect("embeddings") != "openai-embeddings" {
			return "", 0, resources.ErrContract
		}
		endpoint = path
		body, ok := doc.Root().Lookup("body")
		if !ok || body.Kind() != oif.Object {
			return "", 0, resources.ErrContract
		}
		itemModel, ok := get(body, "model")
		if !ok || itemModel != model {
			return "", 0, resources.ErrContract
		}
	}
	if scanner.Err() != nil || len(seen) == 0 || opened.Artifact.ContentLength != part.Size || opened.Artifact.Digest != part.Digest {
		return "", 0, resources.ErrContract
	}
	return endpoint, len(seen), nil
}

func strictResult(body []byte, expectedID string) (oif.Document, string, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: resources.MaxContinuationBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Document{}, "", resources.ErrContract
	}
	id, ok := doc.Root().Lookup("id")
	value, valid := id.Text()
	if !ok || !valid || value == "" || len(value) > 512 || expectedID != "" && value != expectedID {
		return oif.Document{}, "", resources.ErrContract
	}
	return doc, value, nil
}

func batchFileFields() [3]string {
	return [3]string{"input_file_id", "output_file_id", "error_file_id"}
}

func strictBatchResult(body []byte, expectedID string) (oif.Document, string, error) {
	doc, id, err := strictResult(body, expectedID)
	if err != nil {
		return oif.Document{}, "", err
	}
	status, ok := doc.Root().Lookup("status")
	state, valid := status.Text()
	if !ok || !valid || wire.BatchStageOf(state) == wire.BatchUnknown {
		return oif.Document{}, "", resources.ErrContract
	}
	for _, name := range batchFileFields() {
		field, present := doc.Root().Lookup(name)
		if !present || field.Kind() == oif.Null {
			continue
		}
		value, valid := field.Text()
		if !valid || value == "" || len(value) > 512 {
			return oif.Document{}, "", resources.ErrContract
		}
	}
	if counts, present := doc.Root().Lookup("request_counts"); present && counts.Kind() != oif.Null {
		if counts.Kind() != oif.Object {
			return oif.Document{}, "", resources.ErrContract
		}
		for _, name := range []string{"total", "completed", "failed"} {
			if value, present := counts.Lookup(name); present {
				number, err := strconv.ParseInt(value.Raw(), 10, 64)
				if err != nil || number < 0 {
					return oif.Document{}, "", resources.ErrContract
				}
			}
		}
	}
	return doc, id, nil
}

func strictBatchInputMatches(result oif.Document, effective []byte) bool {
	bound, err := oif.ParseJSON(effective, oif.Limits{MaxBytes: resources.MaxContinuationBytes})
	if err != nil || bound.Root().Kind() != oif.Object {
		return false
	}
	expected, present := bound.Root().Lookup("input_file_id")
	fileID, valid := expected.Text()
	if !present || !valid || fileID == "" {
		return false
	}
	if observed, present := result.Root().Lookup("input_file_id"); present && observed.Kind() != oif.Null {
		value, valid := observed.Text()
		return valid && value == fileID
	}
	return true
}

func durableProjection(result []byte, localID string, files map[string]string) ([]byte, error) {
	doc, _, err := strictResult(result, "")
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(localID)
	changes := []oif.Change{{Pointer: "/id", Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped provider resource"}}
	for field, mapped := range files {
		if _, present := doc.Root().Lookup(field); present {
			encoded, _ := json.Marshal(mapped)
			changes = append(changes, oif.Change{Pointer: "/" + field, Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped provider file"})
		}
	}
	out, err := oif.Apply(doc, changes)
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
