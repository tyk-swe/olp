package protocols

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func structuredToolRequest(t *testing.T, family openai.Family, surface, value string) (*openai.Request, string) {
	t.Helper()
	block := `{"toolUse":{"toolUseId":"call","name":"lookup","input":` + value + `}}`
	path := "/messages/0/content/0/toolUse/input"
	if surface == "result" {
		block = `{"toolResult":{"toolUseId":"call","content":[{"json":` + value + `}]}}`
		path = "/messages/0/content/0/toolResult/content/0/json"
	}
	body := `{"messages":[{"role":"user","content":[` + block + `,{"text":"after tool data"}]}]}`
	if family == "bedrock_count" {
		body = `{"input":{"converse":` + body + `}}`
		path = "/input/converse" + path
	}
	return openai.NewEnvelope(family, "route", false, decodeFields(t, body)), path
}

func TestInspectInputTextBedrockStructuredBlock(t *testing.T) {
	cases := []struct{ name, input, pattern string }{
		{"key", `{"forbidden":"safe"}`, "forbidden"},
		{"escaped key", `{"for\u0062idden":"safe"}`, "forbidden"},
		{"number", `{"count":9007199254740993}`, "9007199254740993"},
		{"true", `{"enabled":true}`, "^true$"},
		{"false", `{"enabled":false}`, "^false$"},
		{"null", `{"empty":null}`, "^null$"},
		{"nested array", `{"items":[{"forbidden":0}]}`, "forbidden"},
		{"deep structure", strings.Repeat(`{"nested":`, 80) + `{"forbidden":0}` + strings.Repeat("}", 80), "forbidden"},
	}
	for _, family := range []openai.Family{openai.FamilyBedrock, "bedrock_count"} {
		for _, surface := range []string{"input", "result"} {
			for _, tc := range cases {
				t.Run(string(family)+"/"+surface+"/"+tc.name, func(t *testing.T) {
					r, _ := structuredToolRequest(t, family, surface, tc.input)
					before := r.OIF().Document().Raw()
					re := regexp.MustCompile(tc.pattern)
					blocked := false
					out, err := InspectInputText(r, func(text string) (string, bool) {
						if blocked {
							t.Fatal("inspection continued after block")
						}
						blocked = re.MatchString(text)
						return text, blocked
					})
					if err != nil || out == nil || !blocked {
						t.Fatalf("structured policy did not block: blocked=%v err=%v", blocked, err)
					}
					if r.OIF().Document().Raw() != before {
						t.Fatal("inspection changed the original request")
					}
				})
			}
		}
	}
}

func TestInspectInputTextBedrockStructuredRedact(t *testing.T) {
	cases := []struct{ name, input, pattern, replacement, want string }{
		{"key", `{"s3cr3t":1}`, "s3cr3t", "[MASK]", `{"[MASK]":1}`},
		{"escaped key", `{"s3cr\u0033t":"safe"}`, "s3cr3t", "[MASK]", `{"[MASK]":"safe"}`},
		{"number", `{"count":12345}`, "123", "[MASK]", `{"count":"[MASK]45"}`},
		{"numeric replacement", `{"count":12345}`, "12345", "0", `{"count":0}`},
		{"boolean", `{"enabled":true}`, "true", "[MASK]", `{"enabled":"[MASK]"}`},
		{"boolean replacement", `{"enabled":true}`, "true", "false", `{"enabled":false}`},
		{"null", `{"empty":null}`, "null", "[MASK]", `{"empty":"[MASK]"}`},
		{"replacement escaping", `{"s3cr3t":12345}`, "s3cr3t|12345", "\"\n\\", `{"\"\n\\":"\"\n\\"}`},
		{"preserve types", `{"n":9007199254740993,"e":1e-9,"b":false,"z":null,"a":[],"o":{}}`, "absent", "[MASK]", `{"n":9007199254740993,"e":1e-9,"b":false,"z":null,"a":[],"o":{}}`},
		{"nested array", `{"items":[{"s3cr3t":"s3cr3t"},12345]}`, "s3cr3t|12345", "[MASK]", `{"items":[{"[MASK]":"[MASK]"},"[MASK]"]}`},
	}
	for _, family := range []openai.Family{openai.FamilyBedrock, "bedrock_count"} {
		for _, surface := range []string{"input", "result"} {
			for _, tc := range cases {
				t.Run(string(family)+"/"+surface+"/"+tc.name, func(t *testing.T) {
					r, path := structuredToolRequest(t, family, surface, tc.input)
					before := r.OIF().Document().Raw()
					re := regexp.MustCompile(tc.pattern)
					out := inspectInputText(t, r, func(text string) (string, bool) {
						return re.ReplaceAllLiteralString(text, tc.replacement), false
					})
					value, ok := out.OIF().Document().Lookup(path)
					if !ok {
						t.Fatalf("missing tool data at %s", path)
					}
					decode := func(raw []byte) any {
						t.Helper()
						var v any
						d := json.NewDecoder(bytes.NewReader(raw))
						d.UseNumber()
						if err := d.Decode(&v); err != nil {
							t.Fatal(err)
						}
						return v
					}
					if !reflect.DeepEqual(decode(value.Bytes()), decode([]byte(tc.want))) {
						t.Fatalf("tool data = %s, want %s", value.Raw(), tc.want)
					}
					if r.OIF().Document().Raw() != before {
						t.Fatal("inspection changed the original request")
					}
				})
			}
		}
	}
}

func TestInspectInputTextBedrockStructuredKeyCollisionFailsClosed(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyBedrock, "bedrock_count"} {
		for _, surface := range []string{"input", "result"} {
			for _, input := range []string{`{"s3cr3t":1,"[REDACTED]":2}`, `{"[REDACTED]":1,"s3cr3t":2}`} {
				r, _ := structuredToolRequest(t, family, surface, input)
				out, err := InspectInputText(r, redactSecret)
				if err == nil || out != nil || strings.Contains(err.Error(), "s3cr3t") {
					t.Fatalf("%s/%s: unsafe collision: out=%v err=%v", family, surface, out, err)
				}
			}
		}
	}
}
