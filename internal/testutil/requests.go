package testutil

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/tests/fixtures"
	fidelityfixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

func read(tb testing.TB, files fs.FS, name string) []byte {
	tb.Helper()
	data, err := fs.ReadFile(files, name)
	if err != nil {
		tb.Fatal(err)
	}
	return data
}

// Requests are the request bodies the corpora hold, by name: those of the
// protocol corpus, those of the fidelity corpus, and what the token corpus makes
// of its conversations and texts, each text as the chat message, responses input,
// Anthropic message and Gemini part a client would send it in. It is what the
// parity tests of a codec's readers run old against new on, beside the inputs they
// generate.
func Requests(tb testing.TB) map[string][]byte {
	tb.Helper()
	bodies := map[string][]byte{}
	for _, name := range []string{"protocols/openai-chat-request.json", "protocols/anthropic-messages-request.json", "protocols/gemini-generate-content-request.json"} {
		bodies[name] = read(tb, fixtures.Files, name)
	}
	bodies["fidelity/v1/anthropic-tool-next-request.json"] = read(tb, fidelityfixtures.Files, "v1/anthropic-tool-next-request.json")
	var families []struct {
		Name string          `json:"name"`
		Wire json.RawMessage `json:"wire"`
	}
	if err := json.Unmarshal(read(tb, fixtures.Files, "protocols/selected-operation-families.json"), &families); err != nil {
		tb.Fatal(err)
	}
	for _, f := range families {
		bodies["family/"+f.Name] = f.Wire
	}
	var counterexamples struct {
		Scenarios []map[string]json.RawMessage `json:"scenarios"`
	}
	if err := json.Unmarshal(read(tb, fidelityfixtures.Files, "v1/counterexamples.json"), &counterexamples); err != nil {
		tb.Fatal(err)
	}
	for _, scenario := range counterexamples.Scenarios {
		for _, member := range []string{"source", "native", "transformed_target", "preserved_target"} {
			if raw := scenario[member]; len(raw) > 0 && raw[0] == '{' {
				bodies["counterexample/"+string(scenario["id"])+"/"+member] = raw
			}
		}
	}
	var conversations []struct {
		Name     string          `json:"name"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(read(tb, fixtures.Files, "tokens/framing.json"), &conversations); err != nil {
		tb.Fatal(err)
	}
	for _, c := range conversations {
		bodies["framing/"+c.Name] = []byte(`{"model":"m","messages":` + string(c.Messages) + `}`)
	}
	var texts []struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(read(tb, fixtures.Files, "tokens/o200k_base.json"), &texts); err != nil {
		tb.Fatal(err)
	}
	for _, f := range texts {
		text, _ := json.Marshal(f.Text)
		for shape, body := range map[string]string{
			"chat":      `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":` + string(text) + `}]}`,
			"responses": `{"model":"m","instructions":"s","input":` + string(text) + `}`,
			"anthropic": `{"model":"m","max_tokens":9,"system":"s","messages":[{"role":"user","content":[{"type":"text","text":` + string(text) + `}]}]}`,
			"gemini":    `{"contents":[{"role":"user","parts":[{"text":` + string(text) + `}]}]}`,
		} {
			bodies["text/"+shape+"/"+f.Name] = []byte(body)
		}
	}
	if len(bodies) < 1500 {
		tb.Fatalf("the corpora made %d request bodies", len(bodies))
	}
	return bodies
}

// Documents are every JSON document the corpora hold, by name, and every data
// payload of the streams they hold: what the protocol codecs read of an upstream
// and of a client, whole and one event at a time.
func Documents(tb testing.TB) map[string][]byte {
	tb.Helper()
	documents := map[string][]byte{}
	for _, files := range []fs.FS{fixtures.Files, fidelityfixtures.Files} {
		err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data := read(tb, files, name)
			switch path.Ext(name) {
			case ".json":
				documents[name] = data
			case ".sse":
				for index, line := range bytes.Split(data, []byte("\n")) {
					if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok && strings.HasPrefix(strings.TrimSpace(string(payload)), "{") {
						documents[name+"#"+strconv.Itoa(index)] = bytes.TrimSpace(payload)
					}
				}
			}
			return nil
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
	if len(documents) < 40 {
		tb.Fatalf("the corpora made %d documents", len(documents))
	}
	return documents
}
