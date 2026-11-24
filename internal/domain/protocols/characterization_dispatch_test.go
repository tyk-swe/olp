package protocols_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type probe struct {
	shape    wire.Format
	request  []byte
	response []byte
	stream   []byte
}

func requestBody(format wire.Format) []byte {
	index := slices.IndexFunc(requestFixtures, func(fixture requestFixture) bool {
		return fixture.format == format && fixture.scenario == ""
	})
	return []byte(requestFixtures[index].body)
}

func responseBody(format wire.Format, scenario string) []byte {
	index := slices.IndexFunc(responseFixtures, func(fixture responseFixture) bool {
		return fixture.format == format && fixture.scenario == scenario
	})
	return []byte(responseFixtures[index].body)
}

func streamBody(format wire.Format, scenario string) []byte {
	sources := streamSources()
	source := sources[slices.IndexFunc(sources, func(source streamSource) bool { return source.format == format })]
	return source.scenarios[slices.IndexFunc(source.scenarios, func(candidate streamScenario) bool { return candidate.name == scenario })].body
}

func probes() []probe {
	return slices.Collect(func(yield func(probe) bool) {
		for _, shape := range referenceFormats {
			if !yield(probe{shape, requestBody(shape), responseBody(shape, "tool call"), streamBody(shape, "tool call")}) {
				return
			}
		}
	})
}

type verdict struct {
	Error  *failure `json:"error,omitempty"`
	Result any      `json:"result,omitempty"`
}

func labelled(format wire.Format, output []byte, render func(wire.Format) ([]byte, error)) verdict {
	if same := sameAs(format, format, output, render); same != "" {
		return verdict{Result: "same as " + same}
	}
	return verdict{Result: digest(output)}
}

func described(format wire.Format, output []byte, render func(wire.Format) ([]byte, error)) verdict {
	if same := sameAs(format, format, output, render); same != "" {
		return verdict{Result: "same as " + same}
	}
	return verdict{Result: document(output)}
}

func coarse(err error) *failure {
	classified := classify(err)
	if classified == nil || classified.Type != "request" {
		return classified
	}
	return &failure{Type: classified.Type, Code: classified.Code}
}

func requestOf(format wire.Format, body []byte) (*wire.Request, error) {
	parsed, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return nil, err
	}
	return wire.NewRequest(format, routeModel, false, parsed)
}

func collapsed(label string, entries map[string]any) any {
	distinct := map[string]any{}
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return entries
		}
		distinct[string(encoded)] = entry
	}
	if len(distinct) != 1 || len(entries) < 2 {
		return entries
	}
	for _, entry := range distinct {
		return map[string]any{label: entry}
	}
	return entries
}

func perProbe(run func(probe) any) any {
	out := map[string]any{}
	for _, probe := range probes() {
		out[string(probe.shape)+" shaped"] = run(probe)
	}
	return collapsed("every shape", out)
}

func formatFacts(format wire.Format) any {
	return map[string]any{
		"operation":                     format.Operation(),
		"surface":                       format.Surface(),
		"dialect":                       wire.Dialect(format),
		"workflow_transcript_field":     format.WorkflowTranscriptField(),
		"rewrites_guardrail_stream":     format.CanRewriteGuardrailStreamText(),
		"native_generation_probe":       protocols.NativeGenerationProbe(format),
		"wire_family_by_provider_kind":  wireFamilies(format),
		"native_generation_family_name": nativeFamily(format),
	}
}

func wireFamilies(source wire.Format) map[string]wire.Format {
	out := map[string]wire.Format{}
	for _, kind := range []string{"anthropic", "gemini", "vertex_ai", "bedrock", "other"} {
		out[kind] = protocols.WireFamily(kind, "", source)
	}
	return out
}

func nativeFamily(format wire.Format) string {
	family, ok := protocols.NativeGenerationFamily(wire.Dialect(format))
	if !ok {
		return ""
	}
	return string(family)
}

func parseVerdicts(format wire.Format) any {
	return perProbe(func(probe probe) any {
		request, err := protocols.Parse(format, probe.request, routeModel)
		if err != nil {
			return verdict{Error: coarse(err)}
		}
		return verdict{Result: "accepted as " + string(request.Family)}
	})
}

func parseSourceVerdicts(format wire.Format) any {
	return perProbe(func(probe probe) any {
		if _, err := protocols.ParseSource(format, probe.request); err != nil {
			return verdict{Error: classify(err)}
		}
		return verdict{Result: "accepted"}
	})
}

func encodeSourceVerdicts(format wire.Format) any {
	return perProbe(func(probe probe) any {
		out := map[string]any{}
		for _, target := range []wire.Format{wire.Chat, wire.Anthropic} {
			encode := func(source wire.Format) ([]byte, error) {
				request, err := requestOf(source, probe.request)
				if err != nil {
					return nil, err
				}
				body, _, err := protocols.EncodeTarget(request, target, "", "", upstreamModel, nil)
				return body, err
			}
			body, err := encode(format)
			if err != nil {
				out["to "+string(target)] = verdict{Error: coarse(err)}
				continue
			}
			out["to "+string(target)] = labelled(format, body, encode)
		}
		return collapsed("to chat or anthropic", out)
	})
}

func requestReaders(format wire.Format) any {
	return perProbe(func(probe probe) any {
		read := func(source wire.Format) ([]byte, error) {
			request, err := requestOf(source, probe.request)
			if err != nil {
				return nil, err
			}
			_, pruningErr := protocols.PlanWorkflowPruning(request)
			return json.Marshal(map[string]any{
				"parameters":        slices.Sorted(slices.Values(protocols.ParameterNames(request))),
				"features":          protocols.RequestFeatures(request),
				"text":              protocols.RequestText(request, 1024),
				"structured_output": protocols.StructuredOutputRequested(request),
				"pruning_task":      protocols.WorkflowPruningTask(request, 1024),
				"pruning_plan":      classify(pruningErr),
			})
		}
		output, err := read(format)
		if err != nil {
			return verdict{Error: classify(err)}
		}
		return described(format, output, read)
	})
}

func decodeSourceVerdicts(format wire.Format) any {
	return perProbe(func(probe probe) any {
		out := map[string]any{}
		for _, native := range []bool{false, true} {
			name, decode := "to chat", func(source wire.Format) ([]byte, error) {
				completion, err := protocols.Decode(source, wire.Chat, probe.response, routeModel, "")
				if err != nil {
					return nil, err
				}
				return json.Marshal(unaryOutcome(completion, nil))
			}
			if native {
				name, decode = "to itself", func(source wire.Format) ([]byte, error) {
					completion, err := protocols.Decode(source, source, probe.response, routeModel, "")
					if err != nil {
						return nil, err
					}
					return json.Marshal(unaryOutcome(completion, nil))
				}
			}
			output, err := decode(format)
			if err != nil {
				out[name] = verdict{Error: classify(err)}
				continue
			}
			out[name] = labelled(format, output, decode)
		}
		return collapsed("to chat or itself", out)
	})
}

func streamVerdict(format wire.Format, run func(wire.Format) (streamOutcome, []byte)) verdict {
	render := func(candidate wire.Format) ([]byte, error) {
		outcome, _ := run(candidate)
		return json.Marshal(outcome)
	}
	outcome, raw := run(format)
	if outcome.Error != nil {
		if len(raw) == 0 {
			return verdict{Error: outcome.Error}
		}
		return verdict{Error: outcome.Error, Result: "after emitting " + digest(raw)}
	}
	whole, err := render(format)
	if err != nil {
		return verdict{Error: classify(err)}
	}
	return labelled(format, whole, render)
}

func streamSourceVerdicts(format wire.Format) any {
	return perProbe(func(probe probe) any {
		return streamVerdict(format, func(source wire.Format) (streamOutcome, []byte) {
			return runStream(source, wire.Chat, probe.stream, true)
		})
	})
}

func streamTargetVerdicts(format wire.Format) any {
	return perProbe(func(probe probe) any {
		return streamVerdict(format, func(target wire.Format) (streamOutcome, []byte) {
			return runStream(probe.shape, target, probe.stream, true)
		})
	})
}

func aggregateVerdict(format wire.Format) any {
	aggregate := func(target wire.Format) ([]byte, error) {
		completion, err := protocols.AggregateTranslatedResponses(target, bytes.NewReader(responsesToolStream), eventLimit, routeModel, nil)
		if err != nil {
			return nil, err
		}
		return json.Marshal(unaryOutcome(completion, nil))
	}
	output, err := aggregate(format)
	if err != nil {
		return verdict{Error: classify(err)}
	}
	return labelled(format, output, aggregate)
}

type captureVerdicts struct {
	Unary          map[string]captured `json:"unary"`
	UnaryRejected  []string            `json:"unary_rejected"`
	Stream         map[string]captured `json:"stream"`
	StreamRejected []string            `json:"stream_rejected"`
}

func captureDispatch(format wire.Format) any {
	out := captureVerdicts{Unary: map[string]captured{}, UnaryRejected: []string{}, Stream: map[string]captured{}, StreamRejected: []string{}}
	for _, probe := range probes() {
		shape := string(probe.shape)
		if unary := captureUnary(format, probe.response, true, true); unary.Error == nil {
			out.Unary[shape] = unary
		} else {
			out.UnaryRejected = append(out.UnaryRejected, shape)
		}
		if stream := captureStream(format, streamFrames(probe.shape, probe.stream), true, true); stream.Error == nil {
			out.Stream[shape] = stream
		} else {
			out.StreamRejected = append(out.StreamRejected, shape)
		}
	}
	return out
}

var meaningfulFrames = []struct{ name, frame string }{
	{"anthropic text delta", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n"},
	{"chat text delta", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n"},
	{"completions text", "data: {\"choices\":[{\"index\":0,\"text\":\"Hel\"}]}\n\n"},
	{"gemini text part", "data: {\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n"},
	{"responses function call item", "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"lookup\"}}\n\n"},
}

func meaningfulVerdicts(format wire.Format) any {
	return slices.Collect(func(yield func(string) bool) {
		for _, candidate := range meaningfulFrames {
			if protocols.MeaningfulFrame(format, []byte(candidate.frame)) && !yield(candidate.name) {
				return
			}
		}
	})
}

func workflowVerdicts(format wire.Format) any {
	definitions := func(family wire.Format) ([]byte, error) {
		return protocols.WorkflowDefinitions(family, []protocols.Tool{{Name: "lookup", Description: "Look up a city", Schema: json.RawMessage(toolSchema)}})
	}
	turns := func(completion *wire.Completion) func(wire.Format) ([]byte, error) {
		return func(family wire.Format) ([]byte, error) {
			turns, err := protocols.WorkflowTurns(family, completion, []protocols.WorkflowResult{{CallID: "call_fixture", Content: "sunny"}})
			if err != nil {
				return nil, err
			}
			return json.Marshal(turns)
		}
	}
	body := func(stream []byte) func(wire.Format) ([]byte, error) {
		return func(family wire.Format) ([]byte, error) {
			return protocols.WorkflowStreamBody(family, bytes.NewReader(stream), eventLimit)
		}
	}
	call := []wire.ToolCall{{ID: "call_fixture", Name: "lookup", Arguments: `{"city":"Oslo"}`}}
	out := map[string]verdict{}
	for name, run := range map[string]func(wire.Format) ([]byte, error){
		"definitions":                 definitions,
		"turns without a body":        turns(&wire.Completion{OutputText: "Hello", ToolCalls: call}),
		"turns with a responses body": turns(&wire.Completion{OutputText: "Hello", ToolCalls: call, Body: responseBody(wire.Responses, "tool call")}),
		"turns with a gemini body":    turns(&wire.Completion{OutputText: "Hello", ToolCalls: call, Body: responseBody(wire.Gemini, "tool call")}),
		"anthropic stream body":       body(anthropicToolStream),
		"gemini stream body":          body(geminiToolStream),
	} {
		output, err := run(format)
		if err != nil {
			out[name] = verdict{Error: classify(err)}
			continue
		}
		out[name] = labelled(format, output, run)
	}
	return out
}

const pixel = "iVBORw0KGgo="

var inlineMediaDocuments = []struct{ name, body string }{
	{"anthropic image blocks", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pixel + `"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pixel + `"}}]}]}`},
	{"chat image urls", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + pixel + `"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + pixel + `"}}]}]}`},
	{"decision input data", `{"input":[{"data":"` + pixel + `"},{"data":"` + pixel + `"}]}`},
	{"gemini inline data", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + pixel + `"}},{"inlineData":{"mimeType":"image/png","data":"` + pixel + `"}}]}]}`},
	{"gemini count inline data", `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + pixel + `"}},{"inlineData":{"mimeType":"image/png","data":"` + pixel + `"}}]}]}}`},
	{"responses input images", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + pixel + `"},{"type":"input_image","image_url":"data:image/png;base64,` + pixel + `"}]}]}`},
}

type inlineMediaVerdicts struct {
	Limited          []string `json:"limited"`
	LimitedByDialect []string `json:"limited_by_dialect"`
}

func inlineMediaDispatch(t *testing.T, format wire.Format) any {
	limits := protocols.InlineMediaLimits{Items: 1, ItemBytes: 1 << 10, TotalBytes: 1 << 10}
	out := inlineMediaVerdicts{Limited: []string{}, LimitedByDialect: []string{}}
	for _, candidate := range inlineMediaDocuments {
		request, err := requestOf(format, []byte(candidate.body))
		if err != nil {
			t.Fatalf("the %s document no longer builds a %s request: %v", candidate.name, format, err)
		}
		if protocols.ValidateInlineMedia(request, limits) != nil {
			out.Limited = append(out.Limited, candidate.name)
		}
		if protocols.ValidateOperationInlineMedia(request.OIF().Document(), oif.Identity{ID: wire.Dialect(format)}, limits) != nil {
			out.LimitedByDialect = append(out.LimitedByDialect, candidate.name)
		}
	}
	return out
}

func embeddingDefaultVerdicts(t *testing.T, format wire.Format) any {
	out := map[string]verdict{}
	for _, source := range []struct {
		name   string
		body   string
		target wire.Format
	}{
		{"one input", singleEmbeddingsRequest, wire.GeminiEmbeddings},
		{"two inputs", embeddingsRequest, wire.GeminiEmbeddings},
	} {
		request, err := parseFixture(wire.Embeddings, source.body)
		if err != nil {
			t.Fatalf("the embeddings fixture no longer parses: %v", err)
		}
		prepared, _, err := protocols.PrepareTarget(request, source.target, "", "", upstreamModel, nil)
		if err != nil {
			t.Fatalf("the embeddings fixture no longer prepares: %v", err)
		}
		filled, err := protocols.ApplyEmbeddingDefaults(prepared, format, protocols.Object{"outputDimensionality": json.RawMessage("8")})
		if err != nil {
			out[source.name] = verdict{Error: classify(err)}
			continue
		}
		if bytes.Equal(filled.Document().Bytes(), prepared.Document().Bytes()) {
			out[source.name] = verdict{Result: "unchanged"}
			continue
		}
		out[source.name] = verdict{Result: document(filled.Document().Bytes())}
	}
	return out
}

func TestFormatDispatch(t *testing.T) {
	t.Parallel()
	columns := []struct {
		name string
		run  func(*testing.T, wire.Format) any
	}{
		{"facts", func(_ *testing.T, format wire.Format) any { return formatFacts(format) }},
		{"parse", func(_ *testing.T, format wire.Format) any { return parseVerdicts(format) }},
		{"parse source", func(_ *testing.T, format wire.Format) any { return parseSourceVerdicts(format) }},
		{"request readers", func(_ *testing.T, format wire.Format) any { return requestReaders(format) }},
		{"encode as source", func(_ *testing.T, format wire.Format) any { return encodeSourceVerdicts(format) }},
		{"decode as source", func(_ *testing.T, format wire.Format) any { return decodeSourceVerdicts(format) }},
		{"stream as source", func(_ *testing.T, format wire.Format) any { return streamSourceVerdicts(format) }},
		{"stream as target", func(_ *testing.T, format wire.Format) any { return streamTargetVerdicts(format) }},
		{"aggregate as target", func(_ *testing.T, format wire.Format) any { return aggregateVerdict(format) }},
		{"capture", func(_ *testing.T, format wire.Format) any { return captureDispatch(format) }},
		{"meaningful frames", func(_ *testing.T, format wire.Format) any { return meaningfulVerdicts(format) }},
		{"workflow", func(_ *testing.T, format wire.Format) any { return workflowVerdicts(format) }},
		{"inline media", inlineMediaDispatch},
		{"embedding defaults", embeddingDefaultVerdicts},
	}
	var cases []goldenCase
	for _, column := range columns {
		for _, format := range formats(t) {
			cases = append(cases, goldenCase{name: column.name + "/" + string(format), run: func(t *testing.T) any { return column.run(t, format) }})
		}
	}
	checkGolden(t, "testdata/format_dispatch.golden.json", cases)
}
