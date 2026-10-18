package routes

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestCertifiedOperationsIncludesDecisions(t *testing.T) {
	model := routing.RevisionModel{Capabilities: []routing.RevisionCapability{{Operation: "decision", Surface: "openai", Mode: "unary", Source: "certified"}}}
	if got := certifiedOperations(model); !slices.Equal(got, []string{"decision"}) {
		t.Fatalf("decision template operations: %v", got)
	}
}
