package protocols_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/tests/fidelity"
	fixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

func TestOrderedGenerationViewUsesIndependentNativeSource(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Replace(data, []byte(`"fixture-model"`), []byte(`"fidelity-route"`), 1)
	r, err := protocols.Parse(openai.FamilyAnthropic, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := protocols.GenerationRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 3 || len(view.Messages[1].Nodes) != 5 {
		t.Fatal("native boundaries lost")
	}
	want := []string{"thinking", "text", "tool_call", "tool_call", "text"}
	for i, kind := range want {
		if view.Messages[1].Nodes[i].Kind != kind {
			t.Fatalf("ordered block %d changed", i)
		}
	}
	for i, id := range []string{"call-weather", "call-clock"} {
		n := view.Messages[2].Nodes[i]
		if n.CallID != id || len(n.Dependencies) != 1 || n.Dependencies[0].ID != id {
			t.Fatal("parallel result dependency lost")
		}
	}
	prepared, err := protocols.PrepareIdentity(r, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := fidelity.Compare(data, prepared.Document().Bytes()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, r.OIF().Source().Bytes()) {
		t.Fatal("native source was modified")
	}
	// A consumer can rearrange a derived view, but never the authoritative source.
	view.Messages[1].Nodes[1], view.Messages[1].Nodes[4] = view.Messages[1].Nodes[4], view.Messages[1].Nodes[1]
	if err := fidelity.Compare(data, prepared.Document().Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationViewsRetainScopesPresenceAndToolSchema(t *testing.T) {
	r, err := protocols.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"system","content":"one"},{"role":"developer","content":"two"},{"role":"user","content":"hi"}],"max_completion_tokens":64,"temperature":null,"parallel_tool_calls":false,"seed":0,"stop":[],"tools":[{"type":"function","function":{"name":"lookup","description":"contract","strict":true,"parameters":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{}}}}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := protocols.GenerationRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Messages[0].Role != "system" || v.Messages[1].Role != "developer" {
		t.Fatal("instruction hierarchy collapsed")
	}
	controls := map[string]oif.Presence{}
	for _, c := range v.Controls {
		controls[c.Name] = c.Presence
		if c.Name == "max_completion_tokens" && (c.Units != "tokens" || c.BudgetScope != "openai-chat/max_completion_tokens") {
			t.Fatal("token scope lost")
		}
	}
	for name, presence := range map[string]oif.Presence{"temperature": oif.ExplicitNull, "parallel_tool_calls": oif.Present, "seed": oif.Present, "stop": oif.Present, "top_p": oif.Missing} {
		if controls[name] != presence {
			t.Fatalf("presence changed: %s", name)
		}
	}
	if len(v.Tools) != 1 || v.Tools[0].Description != "contract" || v.Tools[0].Strictness.Raw() != "true" || v.Tools[0].SchemaDialect != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatal("tool schema contract lost")
	}
}

func TestOIFResultRetainsNativeCandidatesAndOpaqueState(t *testing.T) {
	native := []byte(`{"id":"native-id","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":1,"message":{"role":"assistant","content":"second","annotations":[{"native":9007199254740993}]},"finish_reason":"stop"},{"index":0,"message":{"role":"assistant","content":null,"refusal":"declined"},"finish_reason":"stop"}],"native_extension":{"opaque":"\u0061","precise":1e400}}`)
	c, err := protocols.Decode(openai.FamilyChat, openai.FamilyChat, native, "route", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Native.Source().Bytes(), native) {
		t.Fatal("upstream result was rewritten before lifting")
	}
	v, err := protocols.GenerationResult(c.Native)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Candidates) != 2 || v.Candidates[0].Index != 1 || v.Candidates[1].Index != 0 || v.Candidates[1].Nodes[0].Kind != "refusal" {
		t.Fatal("candidate order or refusal lost")
	}
	if err := fidelity.Compare(bytes.Replace(native, []byte(`"model":"upstream"`), []byte(`"model":"route"`), 1), c.Body); err != nil {
		t.Fatal(err)
	}
}

func TestNativeStreamLiftsBeforeProjectionAndDoesNotCollectHistory(t *testing.T) {
	source, err := fixtures.Files.ReadFile("v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	count := uint64(0)
	sawModel, sawSignature := false, false
	c, err := protocols.StreamWithEvents(openai.FamilyAnthropic, openai.FamilyAnthropic, bytes.NewReader(source), 8192, "route", true, func(frame []byte) error { out.Write(frame); return nil }, func(e oif.Event) error {
		if e.Sequence() != count {
			t.Fatal("event position changed")
		}
		count++
		if value, ok := e.Source().Lookup("/message/model"); ok {
			sawModel = value.Raw() == `"fixture-model"`
		}
		if _, ok := e.Source().Lookup("/delta/signature"); ok {
			sawSignature = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 19 || !sawModel || !sawSignature {
		t.Fatal("native event lifting missed source state", count)
	}
	if c.Native.Source().Valid() || c.OutputText != "" {
		t.Fatal("native stream accumulated a complete source history")
	}
	expected := bytes.ReplaceAll(source, []byte(`"model":"fixture-model"`), []byte(`"model":"route"`))
	if err := fidelity.CompareEvents(expected, out.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguousUpstreamNativeSourceNeverProducesSuccess(t *testing.T) {
	for _, value := range []string{`{"x":1,"x":2}`, `{"x":"\ud800"}`} {
		body := `{"id":"c","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"opaque":` + value + `}`
		if _, err := protocols.Decode(openai.FamilyChat, openai.FamilyChat, []byte(body), "route", ""); err == nil {
			t.Fatal("ambiguous unary source accepted")
		}
		stream := `data: {"id":"c","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"opaque":` + value + "}\n\ndata: [DONE]\n\n"
		var out bytes.Buffer
		_, err := protocols.Stream(openai.FamilyChat, openai.FamilyChat, strings.NewReader(stream), 8192, "route", true, func(frame []byte) error { out.Write(frame); return nil })
		if err == nil || strings.Contains(out.String(), "[DONE]") {
			t.Fatal("ambiguous stream source produced success")
		}
	}
}

func TestExplicitDestinationNeverFallsBackToAnotherDialect(t *testing.T) {
	r, err := protocols.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"hi"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	_, wire, err := protocols.EncodeTarget(r, openai.FamilyResponses, "openai", "openai", "model", nil)
	if err == nil || wire != openai.FamilyResponses {
		t.Fatal("explicit Responses target fell back to Chat")
	}
	var refusal *openai.RequestError
	if !errors.As(err, &refusal) {
		t.Fatal("missing structured incompatibility")
	}
	response, err := protocols.Parse(openai.FamilyResponses, []byte(`{"model":"route","input":"hi","native_option":{"n":9007199254740993}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := protocols.PrepareIdentity(response, "model")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(identity.Document().Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["input"]) != `"hi"` {
		t.Fatal("identity normalized the source input form")
	}
	legacy, _, err := protocols.PrepareTarget(response, openai.FamilyResponses, "openai", "openai", "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Identity() || legacy.Provenance()[0].Origin != oif.LegacyMapping {
		t.Fatal("legacy normalization claimed identity")
	}
}

func TestIdentityTransportOverlayPreservesOtherNativeOptions(t *testing.T) {
	raw := `{"model":"route","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":false,"future": { "opaque":"\u0061", "n":1e400 }}}`
	r, err := protocols.Parse(openai.FamilyChat, []byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := protocols.PrepareIdentity(r, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	original, _ := r.OIF().Source().Lookup("/stream_options/future")
	actual, _ := p.Document().Lookup("/stream_options/future")
	if original.Raw() != actual.Raw() {
		t.Fatal("accounting overlay changed a native option")
	}
	if usage, _ := p.Document().Lookup("/stream_options/include_usage"); usage.Raw() != "true" {
		t.Fatal("accounting usage option absent")
	}
}

func TestPreparedDefaultsReportActualBranches(t *testing.T) {
	for _, test := range []struct {
		name, field string
		want        bool
	}{{"absent", "", true}, {"explicit-null", `,"temperature":null`, false}, {"equal-caller-value", `,"temperature":0`, false}} {
		t.Run(test.name, func(t *testing.T) {
			r, err := protocols.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"hi"}]`+test.field+`}`), "")
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := protocols.PrepareTarget(r, openai.FamilyChat, "openai", "openai", "upstream", protocols.Object{"temperature": json.RawMessage("0")})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range p.Provenance() {
				found = found || entry.Pointer == "/temperature" && entry.Origin == oif.ProviderDefault
			}
			if found != test.want {
				t.Fatal("default provenance was inferred from equal output instead of application")
			}
		})
	}
	r, err := protocols.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"hi"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := protocols.PrepareTarget(r, openai.FamilyAnthropic, "anthropic", "anthropic", "upstream", protocols.Object{"max_tokens": json.RawMessage("32")})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range p.Provenance() {
		found = found || entry.Pointer == "/max_tokens" && entry.Origin == oif.ProviderDefault
	}
	if !found {
		t.Fatal("translated native token default not recorded")
	}
}
