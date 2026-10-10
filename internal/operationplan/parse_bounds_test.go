package operationplan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
)

func TestNativeOperationRejectsNonObjectBeforeIndexing(t *testing.T) {
	small := []byte("[]")
	large := []byte(" \n[" + strings.Repeat("0,", 1<<18) + "0]\t")
	for _, dialect := range []string{"openai-embeddings", "gemini-embeddings", "openai-input-tokens", "rerank"} {
		t.Run(dialect, func(t *testing.T) {
			var failure error
			baseline := testing.AllocsPerRun(2, func() { _, failure = operationplan.Parse(dialect, small, 1<<20) })
			allocations := testing.AllocsPerRun(2, func() { _, failure = operationplan.Parse(dialect, large, 1<<20) })
			var invalid *oif.Incompatibility
			if !errors.As(failure, &invalid) || invalid.Code != "unsupported_parameter" || invalid.Requirement != "native_request" {
				t.Fatalf("expected an invalid native envelope: %v", failure)
			}
			// Dialect lookup has its own fixed schema allocations; invalid body
			// size must not add the recursive source index's allocations.
			if allocations > baseline+2 {
				t.Fatalf("non-object body allocated a source index: small=%v large=%v", baseline, allocations)
			}
		})
	}
}

func TestNativeOperationEnvelopeGuardPreservesObjectSource(t *testing.T) {
	body := []byte(" \n{\"model\":\"route\",\"input\":[\"a\"],\"native\":{\"number\":9007199254740993,\"zero\":-0}}\t")
	request, err := operationplan.Parse("openai-embeddings", body, len(body))
	if err != nil || request.Source().Raw() != string(body) {
		t.Fatalf("valid native source was changed: %v", err)
	}
	for _, input := range []string{"", "null", "true", "0", `"text"`, "[]"} {
		if _, err := operationplan.Parse("openai-embeddings", []byte(input), 1024); err == nil {
			t.Fatalf("accepted non-object source %q", input)
		}
	}
	for _, input := range []string{`{"x":0,"x":1}`, `{"x":`, `{} {}`} {
		if _, err := operationplan.Parse("openai-embeddings", []byte(input), 1024); err == nil {
			t.Fatalf("accepted malformed object %q", input)
		}
	}
	if _, err := operationplan.Parse("openai-embeddings", body, len(body)-1); err == nil {
		t.Fatal("object envelope bypassed its byte bound")
	}
}
