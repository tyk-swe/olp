package connectors

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
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

// Events reach the codec no larger than the upstream sent them, so an event
// within the limit stays within it.

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

// A default the request has and a deletion of a member it lacks change
// nothing; sets always apply.

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
