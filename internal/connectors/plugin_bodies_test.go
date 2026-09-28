package connectors

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// envelopedManifest declares a Gemini profile whose upstream wraps each
// request as {model, project, request} and each response and stream event as
// {response}, and rewrites the dialect's request.
func envelopedManifest() abi.Manifest {
	manifest := pluginManifest()
	manifest.Profiles[0].Dialect = "gemini-generate-content"
	manifest.Profiles[0].Hosting = abi.Hosting{
		Address:  "https://api.acme.example/v2",
		Headers:  map[string]string{"Authorization": "Bearer {credential}"},
		Envelope: &abi.Envelope{Request: "request", Fields: map[string]string{"model": "{model}", "project": "olp"}, Response: "response"},
		Rewrites: []abi.Rewrite{
			{Op: abi.RewriteSet, Path: "/generationConfig/candidateCount", Value: json.RawMessage(`1`)},
			{Op: abi.RewriteDefault, Path: "/systemInstruction", Value: json.RawMessage(`{"parts":[{"text":"Be brief."}]}`)},
			{Op: abi.RewriteDelete, Path: "/generationConfig/seed"},
		},
	}
	return manifest
}

func TestPluginEnvelopeWrapsRequestsAndUnwrapsResponses(t *testing.T) {
	c := pluginConfig(t, envelopedManifest())
	c.Bindings = map[string]Binding{"acme": {Model: "acme-large-002"}}
	wrapped := c.WrapRequest([]byte(`{"contents":[{"parts":[{"text":"<hi>"}]}]}`), "acme")
	if want := `{"model":"acme-large-002","project":"olp","request":{"contents":[{"parts":[{"text":"<hi>"}]}]}}`; string(wrapped) != want {
		t.Fatalf("wrapped %s, want %s", wrapped, want)
	}
	for body, want := range map[string]string{
		`{"response":{"candidates":[]},"traceId":"t-1"}`: `{"candidates":[]}`,
		`{"response": [1, 2]}`:                           `[1, 2]`,
		`{"error":{"code":400,"message":"no"}}`:          `{"error":{"code":400,"message":"no"}}`,
		`[{"response":{}}]`:                              `[{"response":{}}]`,
		`not json`:                                       `not json`,
	} {
		if got := c.UnwrapResponse([]byte(body)); string(got) != want {
			t.Errorf("unwrapped %s to %s, want %s", body, got, want)
		}
	}

	requestOnly := envelopedManifest()
	requestOnly.Profiles[0].Hosting.Envelope.Response = ""
	if c = pluginConfig(t, requestOnly); string(c.UnwrapResponse([]byte(`{"response":{}}`))) != `{"response":{}}` {
		t.Fatal("unwrapped a response the profile reads as it is")
	}
	plain := pluginConfig(t, pluginManifest())
	if string(plain.WrapRequest([]byte(`{"messages":[]}`), "acme")) != `{"messages":[]}` || string(plain.UnwrapResponse([]byte(`{"response":{}}`))) != `{"response":{}}` {
		t.Fatal("a profile without an envelope changed a body")
	}
}

// Envelope fields may carry the provider's options, filled from its values as
// hosting's headers and query parameters are.
func TestPluginEnvelopeFieldsCarryTheProvidersOptions(t *testing.T) {
	manifest := envelopedManifest()
	manifest.Profiles[0].Options = []abi.Option{{Name: "project", Label: "Project"}}
	manifest.Profiles[0].Hosting.Envelope.Fields["project"] = "projects/{options.project}"
	c := pluginConfig(t, manifest)
	c.PluginOptions = map[string]string{"project": `acme "prod"`}
	wrapped := c.WrapRequest([]byte(`{"contents":[]}`), "acme-large")
	if want := `{"model":"acme-large","project":"projects/acme \"prod\"","request":{"contents":[]}}`; string(wrapped) != want {
		t.Fatalf("wrapped %s, want %s", wrapped, want)
	}
}

func TestPluginEnvelopeUnwrapsEachStreamEvent(t *testing.T) {
	c := pluginConfig(t, envelopedManifest())
	upstream := "event: chunk\nid: 7\ndata: {\"response\":{\"n\":1},\"traceId\":\"t-1\"}\n\n" +
		": keep-alive\n\n" +
		"retry: 250\r\ndata: {\"response\":\r\ndata: {\"n\":2}}\r\n\r\n" +
		"data: {\"error\":{\"code\":429}}\n\n" +
		"data: [DONE]\n\n"
	var events []sse.Frame
	if err := sse.Decode(c.StreamPayload(testutil.Fragmented([]byte(upstream), 3), 1024), 1024, func(f sse.Frame) error {
		events = append(events, f)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	name, id, retry := "chunk", "7", uint64(250)
	want := []sse.Frame{
		{Event: &name, ID: &id, Data: `{"n":1}`},
		{ID: &id, RetryMS: &retry, Data: `{"n":2}`},
		{ID: &id, Data: `{"error":{"code":429}}`},
		{ID: &id, Data: `[DONE]`},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unwrapped %+v, want %+v", events, want)
	}

	// Events reach the codec no larger than the upstream sent them, so an event
	// within the limit stays within it.
	longID := strings.Repeat("i", 40)
	long := "data:{\"response\":\"" + strings.Repeat("x", 90) + "\"}\n\n"
	passed := "data:[" + strings.Repeat("1,", 50) + "1]\n\n"
	limit := max(len(long), len(passed))
	events = nil
	if err := sse.Decode(c.StreamPayload(strings.NewReader("id:"+longID+"\ndata:{\"response\":1}\n\n"+long+passed), limit), limit, func(f sse.Frame) error {
		events = append(events, f)
		return nil
	}); err != nil || len(events) != 3 || *events[2].ID != longID || events[1].Data != `"`+strings.Repeat("x", 90)+`"` {
		t.Fatalf("events at the limit: %+v %v", events, err)
	}

	oversized := "data: {\"response\":\"" + strings.Repeat("x", 64) + "\"}\n\n"
	if _, err := io.ReadAll(c.StreamPayload(strings.NewReader(oversized), 32)); !errors.Is(err, sse.ErrEventTooLarge) {
		t.Fatalf("read an event beyond the limit: %v", err)
	}
	stream := strings.NewReader("data: {\"response\":{}}\n\n")
	if plain := pluginConfig(t, pluginManifest()); plain.StreamPayload(stream, 1024) != stream {
		t.Fatal("a profile without an envelope reframed its stream")
	}
}

func TestPluginEnvelopePreservesSeparateIDFramesAtEventLimit(t *testing.T) {
	c := pluginConfig(t, envelopedManifest())
	payload := `{"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("x", 64) + `"}]}}]}`
	dataFrame := "data:{\"response\":" + payload + "}\n\n"
	id := strings.Repeat("i", len(dataFrame)-len("id:\n\n"))
	for name, prior := range map[string]string{"initial ID": "", "changed ID": "id:old\ndata:{\"response\":{}}\n\n"} {
		t.Run(name, func(t *testing.T) {
			upstream := prior + "id:" + id + "\n\n" + dataFrame + "id:\n\n" + dataFrame
			for _, width := range []int{1, len(upstream)} {
				var events []sse.Frame
				err := sse.Decode(c.StreamPayload(testutil.Fragmented([]byte(upstream), width), len(dataFrame)), len(dataFrame), func(frame sse.Frame) error {
					events = append(events, frame)
					return nil
				})
				if err != nil {
					t.Fatalf("width=%d: rejected frames within the event limit: %v", width, err)
				}
				emptyID := ""
				want := []sse.Frame{{ID: &id, Data: payload}, {ID: &emptyID, Data: payload}}
				if prior != "" {
					oldID := "old"
					want = append([]sse.Frame{{ID: &oldID, Data: `{}`}}, want...)
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("width=%d: events=%+v, want %+v", width, events, want)
				}
			}
		})
	}
}

// prepared is a prepared Responses request with body as its document.
func prepared(t *testing.T, body string) oif.Prepared {
	t.Helper()
	document, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := oif.Descriptor{Operation: oif.Identity{ID: "generation", Revision: "1"}, Dialect: oif.Identity{ID: "openai-responses", Revision: "1"}}
	request, err := oif.NewRequest(descriptor, document)
	if err != nil {
		t.Fatal(err)
	}
	p, err := oif.PrepareDestination(request, descriptor, document, oif.TransformedMapping, "test preparation")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginRewritesSetDefaultAndDeleteDeclaredMembers(t *testing.T) {
	manifest := pluginManifest()
	manifest.Profiles[0].Dialect = "openai-responses"
	manifest.Profiles[0].Hosting.Rewrites = []abi.Rewrite{
		{Op: abi.RewriteSet, Path: "/store", Value: json.RawMessage(`false`)},
		{Op: abi.RewriteDefault, Path: "/instructions", Value: json.RawMessage(`"Be brief."`)},
		{Op: abi.RewriteDelete, Path: "/max_output_tokens"},
		{Op: abi.RewriteSet, Path: "/reasoning/effort", Value: json.RawMessage(`"low"`)},
		{Op: abi.RewriteSet, Path: "/metadata/client/name", Value: json.RawMessage(`null`)},
	}
	c := pluginConfig(t, manifest)
	for name, tc := range map[string]struct {
		body, want string
		changed    []string
	}{
		"members the request lacks": {
			`{"model":"m","input":"hi","max_output_tokens":5}`,
			`{"model":"m","input":"hi","store":false,"instructions":"Be brief.","reasoning":{"effort":"low"},"metadata":{"client":{"name":null}}}`,
			[]string{"", "/store", "/instructions", "/max_output_tokens", "/reasoning", "/metadata"},
		},
		"members the request has": {
			`{"model":"m","store":true,"instructions":"Mine.","input":"hi","reasoning":{"summary":"auto","effort":"high"},"metadata":{"client":{}}}`,
			`{"model":"m","store":false,"instructions":"Mine.","input":"hi","reasoning":{"summary":"auto","effort":"low"},"metadata":{"client":{"name":null}}}`,
			[]string{"", "/store", "/reasoning/effort", "/metadata/client/name"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			rewritten, err := c.Rewrite(prepared(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := rewritten.Document().Raw(); got != tc.want {
				t.Fatalf("rewrote to %s, want %s", got, tc.want)
			}
			changed := []string{}
			for _, entry := range rewritten.Provenance() {
				if entry.Origin == hostingRewrite {
					changed = append(changed, entry.Pointer)
				}
			}
			if !reflect.DeepEqual(changed, tc.changed) {
				t.Fatalf("recorded rewrites of %q, want %q", changed, tc.changed)
			}
		})
	}

	// A default the request has and a deletion of a member it lacks change
	// nothing; sets always apply.
	satisfied := prepared(t, `{"model":"m","input":"hi","store":false,"instructions":"Mine.","reasoning":{"effort":"low"},"metadata":{"client":{"name":null}}}`)
	unchanged, err := c.Rewrite(satisfied)
	if err != nil || unchanged.Document().Raw() != satisfied.Document().Raw() {
		t.Fatalf("a request that satisfies every rewrite changed: %s %v", unchanged.Document().Raw(), err)
	}
	for _, entry := range unchanged.Provenance() {
		if entry.Pointer == "/instructions" || entry.Pointer == "/max_output_tokens" {
			t.Fatalf("recorded a rewrite that changed nothing: %+v", entry)
		}
	}
	if same, err := pluginConfig(t, pluginManifest()).Rewrite(satisfied); err != nil || !reflect.DeepEqual(same, satisfied) {
		t.Fatalf("a profile without rewrites changed a request: %v", err)
	}
	if _, err := c.Rewrite(prepared(t, `{"model":"m","input":"hi","reasoning":"high"}`)); err == nil || !strings.Contains(err.Error(), "/reasoning to be an object") {
		t.Fatalf("placed a rewrite inside a request value that is not an object: %v", err)
	}
}

func TestPluginProfilesWithBodyChangesAreTransformedOnly(t *testing.T) {
	for name, mutate := range map[string]func(*abi.Hosting){
		"envelope": func(h *abi.Hosting) { h.Envelope = &abi.Envelope{Response: "response"} },
		"rewrite": func(h *abi.Hosting) {
			h.Rewrites = []abi.Rewrite{{Op: abi.RewriteDelete, Path: "/user"}}
		},
	} {
		manifest := pluginManifest()
		mutate(&manifest.Profiles[0].Hosting)
		c := pluginConfig(t, manifest)
		if p, _ := c.Profile(); p.Strict {
			t.Errorf("a profile with a declared %s serves strict routes", name)
		}
		decoded, encoded := publish(t, c)
		if !reflect.DeepEqual(decoded.Plugin.hosting, c.Plugin.hosting) {
			t.Errorf("the %s did not survive publication: %s", name, encoded)
		}
	}
}

func TestPluginEnvelopeAndRewriteValidationLocatesTheOffendingValue(t *testing.T) {
	if err := ValidatePluginProfile(envelopedManifest().Profiles[0]); err != nil {
		t.Fatal(err)
	}
	fields, rewrites := map[string]string{}, []abi.Rewrite{}
	for _, name := range strings.Fields("a b c d e f g h i j k l m n o p q") {
		fields[name] = "olp"
		rewrites = append(rewrites, abi.Rewrite{Op: abi.RewriteDelete, Path: "/" + name})
	}
	for name, tc := range map[string]struct {
		mutate func(*abi.Profile)
		field  string
	}{
		"empty envelope":      {func(p *abi.Profile) { p.Hosting.Envelope = &abi.Envelope{} }, "hosting.envelope"},
		"request member":      {func(p *abi.Profile) { p.Hosting.Envelope.Request = "the request" }, "hosting.envelope.request"},
		"response member":     {func(p *abi.Profile) { p.Hosting.Envelope.Response = "/response" }, "hosting.envelope.response"},
		"fields without body": {func(p *abi.Profile) { p.Hosting.Envelope.Request = "" }, "hosting.envelope.fields"},
		"too many fields":     {func(p *abi.Profile) { p.Hosting.Envelope.Fields = fields }, "hosting.envelope.fields"},
		"field name":          {func(p *abi.Profile) { p.Hosting.Envelope.Fields["a b"] = "olp" }, "hosting.envelope.fields.a b"},
		"field is the body":   {func(p *abi.Profile) { p.Hosting.Envelope.Fields["request"] = "olp" }, "hosting.envelope.fields.request"},
		"credential in body":  {func(p *abi.Profile) { p.Hosting.Envelope.Fields["key"] = "{credential}" }, "hosting.envelope.fields.key"},
		"unknown placeholder": {func(p *abi.Profile) { p.Hosting.Envelope.Fields["project"] = "{project}" }, "hosting.envelope.fields.project"},
		"undeclared option":   {func(p *abi.Profile) { p.Hosting.Envelope.Fields["project"] = "{options.project}" }, "hosting.envelope.fields.project"},
		"optional option": {func(p *abi.Profile) {
			p.Options = []abi.Option{{Name: "project", Label: "Project", Optional: true}}
			p.Hosting.Envelope.Fields["project"] = "{options.project}"
		}, "hosting.envelope.fields.project"},
		"field control":     {func(p *abi.Profile) { p.Hosting.Envelope.Fields["project"] = "olp\n" }, "hosting.envelope.fields.project"},
		"model in a header": {func(p *abi.Profile) { p.Hosting.Headers["X-Model"] = "{model}" }, "hosting.headers.X-Model"},
		"too many rewrites": {func(p *abi.Profile) { p.Hosting.Rewrites = rewrites }, "hosting.rewrites"},
		"unknown op":        {func(p *abi.Profile) { p.Hosting.Rewrites[0].Op = "replace" }, "hosting.rewrites[0].op"},
		"set without value": {func(p *abi.Profile) { p.Hosting.Rewrites[0].Value = nil }, "hosting.rewrites[0].value"},
		"invalid value":     {func(p *abi.Profile) { p.Hosting.Rewrites[1].Value = json.RawMessage(`{`) }, "hosting.rewrites[1].value"},
		"ambiguous value":   {func(p *abi.Profile) { p.Hosting.Rewrites[1].Value = json.RawMessage(`{"a":1,"a":2}`) }, "hosting.rewrites[1].value"},
		"deep value": {func(p *abi.Profile) {
			p.Hosting.Rewrites[0].Value = json.RawMessage(strings.Repeat("[", 40) + strings.Repeat("]", 40))
		}, "hosting.rewrites[0].value"},
		"delete with value":     {func(p *abi.Profile) { p.Hosting.Rewrites[2].Value = json.RawMessage(`1`) }, "hosting.rewrites[2].value"},
		"relative path":         {func(p *abi.Profile) { p.Hosting.Rewrites[0].Path = "store" }, "hosting.rewrites[0].path"},
		"empty member":          {func(p *abi.Profile) { p.Hosting.Rewrites[0].Path = "/generationConfig//seed" }, "hosting.rewrites[0].path"},
		"escaped member":        {func(p *abi.Profile) { p.Hosting.Rewrites[0].Path = "/a~1b" }, "hosting.rewrites[0].path"},
		"deep path":             {func(p *abi.Profile) { p.Hosting.Rewrites[0].Path = "/a/b/c/d/e/f/g/h/i" }, "hosting.rewrites[0].path"},
		"same member twice":     {func(p *abi.Profile) { p.Hosting.Rewrites[2].Path = "/generationConfig/candidateCount" }, "hosting.rewrites[2].path"},
		"inside another member": {func(p *abi.Profile) { p.Hosting.Rewrites[2].Path = "/generationConfig" }, "hosting.rewrites[2].path"},
		"bound model": {func(p *abi.Profile) {
			p.Dialect, p.Hosting.Rewrites = "openai-chat", []abi.Rewrite{{Op: abi.RewriteSet, Path: "/model", Value: json.RawMessage(`"other"`)}}
		}, "hosting.rewrites[0].path"},
		"bound usage reporting": {func(p *abi.Profile) {
			p.Dialect, p.Hosting.Rewrites = "openai-chat", []abi.Rewrite{{Op: abi.RewriteDelete, Path: "/stream_options/include_usage"}}
		}, "hosting.rewrites[0].path"},
		"bound delivery": {func(p *abi.Profile) {
			p.Dialect, p.Hosting.Rewrites = "anthropic-messages", []abi.Rewrite{{Op: abi.RewriteSet, Path: "/stream", Value: json.RawMessage(`true`)}}
		}, "hosting.rewrites[0].path"},
	} {
		t.Run(name, func(t *testing.T) {
			p := envelopedManifest().Profiles[0]
			tc.mutate(&p)
			refusal, ok := errors.AsType[*ProfileError](ValidatePluginProfile(p))
			if !ok || refusal.Field != tc.field {
				t.Fatalf("want a refusal of %s, got %v", tc.field, refusal)
			}
		})
	}
}
