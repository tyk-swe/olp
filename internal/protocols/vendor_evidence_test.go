package protocols

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/vendors"
	"github.com/tyk-swe/olp/tests/fixtures"
)

// vendorEvidence is a preset's reviewed evidence: what its documentation says
// about addressing, refused parameters, usage, stream termination and errors,
// transcribed from documentation_url on reviewed_at. Each fixture states its
// basis: a documented example verbatim, one built from the documented schema,
// one built from the OpenAI dialect the vendor documents compatibility with
// where its documentation is silent, or one observed from the live API.
type vendorEvidence struct {
	Vendor           string `json:"vendor"`
	DocumentationURL string `json:"documentation_url"`
	ReviewedAt       string `json:"reviewed_at"`
	Notes            string `json:"notes"`
	// Endpoints are the documented absolute URLs of every served operation.
	Endpoints map[string]string `json:"endpoints"`
	// MediaModels are the models the media package's endpoint check names.
	MediaModels map[string]string `json:"media_models"`
	Unsupported []string          `json:"unsupported_parameters"`
	// Responses are unary results by operation.
	Responses map[string]struct {
		File  string          `json:"file"`
		Basis string          `json:"basis"`
		Usage json.RawMessage `json:"usage"`
	} `json:"responses"`
	// Stream is a chat stream, when the vendor generates.
	Stream *struct {
		File  string          `json:"file"`
		Basis string          `json:"basis"`
		Text  string          `json:"text"`
		Usage json.RawMessage `json:"usage"`
	} `json:"stream"`
	Error struct {
		File   string `json:"file"`
		Basis  string `json:"basis"`
		Status int    `json:"status"`
		// Envelope is the error the OpenAI envelope parser reads, or null
		// for a body in another shape, which is classified by its status.
		Envelope *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"envelope"`
		Class string `json:"class"`
	} `json:"error"`
}

var generationDialects = []string{"openai-chat", "openai-responses", "anthropic-messages", "gemini-generate-content", "bedrock-converse"}

var evidenceBases = []string{"documented", "schema", "dialect", "observed"}

func checkBasis(t *testing.T, what, basis string) {
	t.Helper()
	if !slices.Contains(evidenceBases, basis) {
		t.Fatalf("%s states basis %q; use one of %v", what, basis, evidenceBases)
	}
}

// sameUsage compares decoded usage with the documented usage by value.
func sameUsage(got *openai.Usage, documented json.RawMessage) bool {
	if got == nil {
		return len(documented) == 0 || string(documented) == "null"
	}
	var want openai.Usage
	if json.Unmarshal(documented, &want) != nil {
		return false
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}

// operationFamilies are the wire families a preset's operations are
// addressed and decoded with.
var operationFamilies = map[string]openai.Family{
	"generation": openai.FamilyChat,
	"embeddings": openai.FamilyEmbeddings,
	"rerank":     openai.FamilyRerank,
}

// unsupportedSamples are valid OpenAI request values for the parameters a
// vendor may refuse, so a refusal comes from its contract, not parsing.
var unsupportedSamples = map[string]any{
	"n": 2, "parallel_tool_calls": false, "user": "u", "store": true, "metadata": map[string]string{"k": "v"},
	"logit_bias": map[string]int{"50256": -100}, "logprobs": true, "top_logprobs": 2, "frequency_penalty": 0.5, "presence_penalty": 0.5,
	"seed": 7, "service_tier": "auto", "prediction": map[string]any{"type": "content", "content": "x"}, "stop": []string{"x"},
	"modalities": []string{"text"}, "response_format": map[string]string{"type": "json_object"}, "reasoning_effort": "low",
	"dimensions": 32, "input_type": "query", "truncate": "END", "truncation": true, "temperature": 0.5, "top_p": 0.5,
	"tool_choice": "none", "audio": map[string]string{"voice": "alloy", "format": "wav"}, "web_search_options": map[string]any{},
}

func readEvidence(t *testing.T, vendor string) vendorEvidence {
	t.Helper()
	raw, err := fs.ReadFile(fixtures.Files, path.Join("vendors", vendor, "contract.json"))
	if err != nil {
		t.Fatalf("%s has no reviewed evidence: %v", vendor, err)
	}
	var evidence vendorEvidence
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		t.Fatalf("%s evidence: %v", vendor, err)
	}
	return evidence
}

func vendorFixture(t *testing.T, vendor, name string) []byte {
	t.Helper()
	raw, err := fs.ReadFile(fixtures.Files, path.Join("vendors", vendor, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestEveryPresetHasReviewedEvidence pins each preset's contract to the
// evidence transcribed from its documentation: a contract change without new
// evidence, or evidence without a contract, fails here.
func TestEveryPresetHasReviewedEvidence(t *testing.T) {
	directories, err := fs.ReadDir(fixtures.Files, "vendors")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, entry := range directories {
		documented = append(documented, entry.Name())
	}
	var presets []string
	for _, contract := range vendors.All() {
		if contract.Preset != nil {
			presets = append(presets, contract.ID)
		}
	}
	slices.Sort(presets)
	if !slices.Equal(documented, presets) {
		t.Fatalf("vendor evidence %v does not match the presets %v", documented, presets)
	}
	for _, contract := range vendors.All() {
		if contract.Preset == nil {
			continue
		}
		t.Run(contract.ID, func(t *testing.T) {
			evidence := readEvidence(t, contract.ID)
			if evidence.Vendor != contract.ID || evidence.DocumentationURL != contract.Documentation.URL || evidence.ReviewedAt == "" {
				t.Fatalf("evidence names %s at %s; the contract documents %s", evidence.Vendor, evidence.DocumentationURL, contract.Documentation.URL)
			}
			checkEndpoints(t, contract, evidence)
			checkRefusals(t, contract, evidence)
			checkResponses(t, contract, evidence)
			checkStream(t, contract, evidence)
			checkError(t, contract, evidence)
			checkProfile(t, contract)
		})
	}
}

func checkEndpoints(t *testing.T, contract vendors.Contract, evidence vendorEvidence) {
	t.Helper()
	var documented []string
	for operation := range evidence.Endpoints {
		documented = append(documented, operation)
	}
	slices.Sort(documented)
	served := slices.Sorted(slices.Values(contract.Operations))
	if !slices.Equal(documented, served) {
		t.Fatalf("documented operations %v; the contract serves %v", documented, served)
	}
	// A preset's provider is addressed through the profile it selects.
	config := connectors.Config{Kind: contract.Connector, AuthMode: contract.Preset.AuthMode, Endpoint: contract.Endpoint, VendorID: contract.ID}
	if ref := contract.Preset.Profile; ref != nil {
		config.ProfileID, config.ProfileRevision = ref.ID, ref.Revision
	}
	for operation, want := range evidence.Endpoints {
		family, ok := operationFamilies[operation]
		if !ok {
			continue
		}
		if target, err := config.TargetFamily(family); config.ProfileID != "" && err == nil {
			family = target
		}
		got, err := config.URL(family, "model", false)
		if profile, profiled := config.Profile(); profiled == nil {
			if codec, registered := operationregistry.Lookup(profile.OperationDialect(operation)); registered && codec.Operation.ID == operation {
				got, err = config.OperationURL(codec, "model")
			}
		}
		if err != nil || got != want {
			t.Fatalf("%s is addressed at %s (%v); the documentation says %s", operation, got, err, want)
		}
	}
}

func checkRefusals(t *testing.T, contract vendors.Contract, evidence vendorEvidence) {
	t.Helper()
	var refused []string
	for _, operation := range contract.Operations {
		refused = append(refused, contract.Request(operation).Unsupported...)
	}
	slices.Sort(refused)
	refused = slices.Compact(refused)
	documented := slices.Sorted(slices.Values(evidence.Unsupported))
	if !slices.Equal(refused, documented) {
		t.Fatalf("the contract refuses %v; the documentation lists %v", refused, documented)
	}
	if !contract.Serves("generation") {
		return
	}
	for _, field := range contract.Request("generation").Unsupported {
		sample, ok := unsupportedSamples[field]
		if !ok {
			t.Fatalf("no sample value for %s", field)
		}
		body, _ := json.Marshal(map[string]any{"model": "route", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, field: sample})
		request, err := Parse(openai.FamilyChat, body, "")
		if err != nil {
			continue // the dialect itself refuses the value
		}
		_, _, err = Encode(request, contract.Connector, contract.ID, "model", nil)
		var refusal *openai.RequestError
		if !errors.As(err, &refusal) || refusal.Param != field {
			t.Fatalf("%s was not refused: %v", field, err)
		}
	}
}

func checkResponses(t *testing.T, contract vendors.Contract, evidence vendorEvidence) {
	t.Helper()
	for operation, documented := range evidence.Responses {
		family, ok := operationFamilies[operation]
		if !ok || !contract.Serves(operation) {
			t.Fatalf("documented %s result for an operation the contract does not serve", operation)
		}
		checkBasis(t, operation+" result", documented.Basis)
		body := vendorFixture(t, contract.ID, documented.File)
		if operation == "generation" {
			family = generationWire(contract)
		}
		var request *openai.Request
		if family == openai.FamilyRerank {
			request, _ = Parse(openai.FamilyRerank, []byte(`{"model":"route","query":"q","documents":["a","b","c"]}`), "")
		}
		completion, err := DecodeRequest(family, family, body, "route", "float", request)
		if err != nil {
			t.Fatalf("%s result does not decode: %v", operation, err)
		}
		if !sameUsage(completion.Usage, documented.Usage) {
			t.Fatalf("%s usage = %+v, documented %s", operation, completion.Usage, documented.Usage)
		}
	}
	if contract.Serves("generation") && len(evidence.Responses["generation"].File) == 0 {
		t.Fatal("a generating vendor needs a chat result")
	}
}

func checkStream(t *testing.T, contract vendors.Contract, evidence vendorEvidence) {
	t.Helper()
	if evidence.Stream == nil {
		if contract.Serves("generation") {
			t.Fatal("a generating vendor needs a chat stream")
		}
		return
	}
	checkBasis(t, "stream", evidence.Stream.Basis)
	stream := vendorFixture(t, contract.ID, evidence.Stream.File)
	var text strings.Builder
	wire := generationWire(contract)
	completion, err := Stream(wire, wire, strings.NewReader(string(stream)), 1<<20, "route", true, func(frame []byte) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(string(frame), "data: "))), &chunk) == nil {
			for _, choice := range chunk.Choices {
				text.WriteString(choice.Delta.Content)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream does not complete: %v", err)
	}
	if text.Len() == 0 {
		// A native dialect's frames are its own; its decoder collects the text.
		text.WriteString(completion.OutputText)
	}
	if text.String() != evidence.Stream.Text {
		t.Fatalf("stream text = %q, documented %q", text.String(), evidence.Stream.Text)
	}
	if !sameUsage(completion.Usage, evidence.Stream.Usage) {
		t.Fatalf("stream usage = %+v, documented %s", completion.Usage, evidence.Stream.Usage)
	}
}

// generationWire is the dialect a preset generates in: its profile's, or Chat
// Completions.
func generationWire(contract vendors.Contract) openai.Family {
	if ref := contract.Preset.Profile; ref != nil {
		config := connectors.Config{Kind: contract.Connector, ProfileID: ref.ID, ProfileRevision: ref.Revision, VendorID: contract.ID}
		if wire, err := config.TargetFamily(openai.FamilyChat); err == nil {
			return wire
		}
	}
	return openai.FamilyChat
}

func checkError(t *testing.T, contract vendors.Contract, evidence vendorEvidence) {
	t.Helper()
	documented := evidence.Error
	checkBasis(t, "error", documented.Basis)
	stated := openai.ParseErrorBody(vendorFixture(t, contract.ID, documented.File))
	switch envelope := documented.Envelope; {
	case envelope == nil && stated != nil:
		t.Fatalf("error body parsed as %+v; the evidence says it is not an OpenAI envelope", stated)
	case envelope != nil && (stated == nil || stated.Type != envelope.Type || stated.Code != envelope.Code || stated.Message != envelope.Message):
		t.Fatalf("error envelope = %+v, documented %+v", stated, envelope)
	}
	// The vendor's declared failure classes apply ahead of the built-in rules.
	declared := connectors.Config{VendorID: contract.ID}.Classification()
	outcome := upstream.Classifier{Declared: declared}.Classify(upstream.Evidence{Reached: true, Status: documented.Status, Error: stated})
	if string(outcome.Class) != documented.Class {
		t.Fatalf("error class = %s, documented %s", outcome.Class, documented.Class)
	}
}

// checkProfile admits a preset profile only for a vendor whose wire is its
// dialect exactly: strict routes pass requests through unshaped.
func checkProfile(t *testing.T, contract vendors.Contract) {
	t.Helper()
	ref := contract.Preset.Profile
	if ref == nil {
		return
	}
	profile, err := connectors.LookupProfile(ref.ID, ref.Revision)
	if err != nil || profile.Kind != contract.Connector || !slices.Contains(profile.Authentication, contract.Preset.AuthMode) || !vendors.Speaks(contract.ID, profile.Dialect) && slices.Contains(generationDialects, profile.Dialect) {
		t.Fatalf("preset profile %s/%s does not compose with the vendor: %v", ref.ID, ref.Revision, err)
	}
	for operation, shape := range contract.Requests {
		if len(shape.Rewrites) > 0 || len(shape.Drop) > 0 {
			t.Fatalf("a strict profile would bypass the reviewed %s request shape", operation)
		}
	}
}
