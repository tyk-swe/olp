package routes

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

// operationSnapshot rebases the estimate snapshot onto a registered unary
// operation served by a bound vendor profile.
func operationSnapshot(models []string, vendor, profile, operation string, strict bool) (*routing.Snapshot, []string) {
	snapshot, ids := estimateSnapshot(models...)
	for id, provider := range snapshot.Providers {
		provider.Kind = "openai_compatible"
		provider.VendorID = vendor
		provider.ProfileID = profile
		provider.ProfileRevision = "1"
		provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: operation, Surface: "openai", Mode: "unary"}}
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes["route"]
	route.Operations = []string{operation}
	route.Fidelity.Mode = routing.FidelityTransformed
	if strict {
		route.Fidelity.Mode = routing.FidelityStrict
	}
	snapshot.Routes["route"] = route
	return snapshot, ids
}

func simulateOperation(t *testing.T, snapshot *routing.Snapshot, input simulationInput, operation string) []inspectedDecision {
	t.Helper()
	input.Operation, input.Surface, input.Mode = operation, "openai", "unary"
	s := &Server{Access: &access.Server{}}
	leg, _, _, err := s.leg(fixedSimulation(snapshot, input, nil), "route", "")
	if err != nil {
		t.Fatal(err)
	}
	return leg.Decisions
}

func TestOCRSimulationAdmitsAndReadsBoundaries(t *testing.T) {
	snapshot, ids := operationSnapshot([]string{"mistral-ocr-latest"}, "mistral", "mistral-ocr", "ocr", true)
	body := `{"model":"route","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`
	decisions := simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(body)}, "ocr")
	d := decisionFor(t, decisions, "mistral-ocr-latest")
	if d.Interaction == nil || d.Interaction.Status != "admitted" || !d.Eligible {
		t.Fatalf("ocr not admitted: %+v", d)
	}

	limit := int64(100)
	for _, id := range ids {
		for i, target := range snapshot.Routes["route"].Targets {
			if target.ID == id {
				provider := snapshot.Providers[target.ProviderID]
				provider.Limits = &routing.Limits{TokensPerMinute: &limit}
				snapshot.Providers[target.ProviderID] = provider
				_ = i
			}
		}
	}
	decisions = simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(body)}, "ocr")
	d = decisionFor(t, decisions, "mistral-ocr-latest")
	if d.Reason == nil || *d.Reason != "unbounded_work_budget" {
		t.Fatalf("token-capped ocr: %+v", d)
	}
}

func TestVertexOCRSimulationAdmitsInlineAndRefusesLocations(t *testing.T) {
	snapshot, _ := estimateSnapshot("processor-1")
	for id, provider := range snapshot.Providers {
		provider.Kind = "vertex_ai"
		provider.VendorID = "google-vertex"
		provider.ProfileID = "vertex-document-processing-ocr"
		provider.ProfileRevision = "1"
		provider.AuthMode = "service_account"
		provider.CloudRegion, provider.CloudProject = "us", "example-project"
		provider.Endpoint = "https://us-documentai.googleapis.com/v1/projects/example-project/locations/us"
		provider.Capabilities = []routing.Capability{{Model: "processor-1", Operation: "ocr", Surface: "openai", Mode: "unary"}}
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes["route"]
	route.Operations = []string{"ocr"}
	route.Fidelity.Mode = routing.FidelityTransformed
	snapshot.Routes["route"] = route

	inline := `{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,JVBERi0xLjQ="}}`
	decisions := simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(inline)}, "ocr")
	d := decisionFor(t, decisions, "processor-1")
	if d.Interaction == nil || d.Interaction.Status != "admitted" || !d.Eligible || d.Interaction.EgressDialect != "vertex-document-processing-ocr" {
		t.Fatalf("inline vertex ocr not admitted: %+v", d)
	}

	remote := `{"model":"route","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`
	decisions = simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(remote)}, "ocr")
	d = decisionFor(t, decisions, "processor-1")
	if d.Interaction != nil && d.Interaction.Status == "admitted" {
		t.Fatalf("provider-fetched https document admitted for vertex ocr: %+v", d)
	}
}

func TestSearchSimulationMapsAndRefusesControls(t *testing.T) {
	snapshot, _ := operationSnapshot([]string{"tavily-model"}, "tavily", "tavily-search", "search", false)
	decisions := simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(`{"query":"q"}`)}, "search")
	d := decisionFor(t, decisions, "tavily-model")
	if d.Interaction == nil || d.Interaction.Status != "admitted" || !d.Eligible || d.Interaction.EgressDialect != "tavily-search" {
		t.Fatalf("search not admitted: %+v", d)
	}

	decisions = simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(`{"query":"q","country":"US"}`)}, "search")
	d = decisionFor(t, decisions, "tavily-model")
	if d.Interaction != nil && d.Interaction.Status == "admitted" {
		t.Fatalf("unsupported control admitted: %+v", d)
	}
}

func TestAzureOCRSimulationMapsAndRefusesControls(t *testing.T) {
	snapshot, _ := operationSnapshot([]string{"prebuilt-layout"}, "azure-document-intelligence", "azure-document-intelligence-ocr", "ocr", false)
	decisions := simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/report.pdf"},"pages":[0,2]}`)}, "ocr")
	d := decisionFor(t, decisions, "prebuilt-layout")
	if d.Interaction == nil || d.Interaction.Status != "admitted" || !d.Eligible || d.Interaction.EgressDialect != "azure-document-intelligence-ocr" {
		t.Fatalf("azure ocr not admitted: %+v", d)
	}

	decisions = simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/report.pdf"},"table_format":"html"}`)}, "ocr")
	d = decisionFor(t, decisions, "prebuilt-layout")
	if d.Interaction != nil && d.Interaction.Status == "admitted" {
		t.Fatalf("unsupported control admitted: %+v", d)
	}
}

func TestBraveSearchSimulationMapsDatesAndRefusesDomains(t *testing.T) {
	snapshot, _ := operationSnapshot([]string{"brave-model"}, "brave", "brave-search", "search", false)
	decisions := simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(`{"query":"q","search_after_date_filter":"01/02/2026","search_before_date_filter":"03/04/2026"}`)}, "search")
	d := decisionFor(t, decisions, "brave-model")
	if d.Interaction == nil || d.Interaction.Status != "admitted" || !d.Eligible || d.Interaction.EgressDialect != "brave-search" {
		t.Fatalf("brave search not admitted: %+v", d)
	}

	for _, body := range []string{
		`{"query":"q","search_domain_filter":["a.com"]}`,
		`{"query":"q","search_after_date_filter":"01/02/2026"}`,
		`{"query":"q","search_recency_filter":"week"}`,
	} {
		decisions = simulateOperation(t, snapshot, simulationInput{Request: json.RawMessage(body)}, "search")
		d = decisionFor(t, decisions, "brave-model")
		if d.Interaction != nil && d.Interaction.Status == "admitted" {
			t.Fatalf("unsupported control admitted for %s: %+v", body, d)
		}
	}
}
