package fidelity_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/tests/fidelity"
	fixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

func TestIndependentOracleDetectsSemanticCorruption(t *testing.T) {
	reference := []byte(`{"messages":[{"role":"system","content":"instruction"},{"role":"assistant","content":[{"type":"text","text":"before"},{"type":"tool_use","id":"call-1","input":{"n":9007199254740993}},{"type":"text","text":"after"}]}],"temperature":null,"parallel_tool_calls":false,"stop":[],"max_completion_tokens":64,"schema":{"name":"answer","description":"contract","strict":true}}`)
	if err := fidelity.Compare(reference, reference); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(map[string]any){
		"delete-control": func(m map[string]any) { delete(m, "max_completion_tokens") },
		"duplicate-content": func(m map[string]any) {
			a := m["messages"].([]any)[1].(map[string]any)
			c := a["content"].([]any)
			a["content"] = append(c, c[1])
		},
		"reorder-content": func(m map[string]any) {
			c := m["messages"].([]any)[1].(map[string]any)["content"].([]any)
			c[1], c[2] = c[2], c[1]
		},
		"relocate-instruction": func(m map[string]any) { m["messages"].([]any)[0].(map[string]any)["role"] = "user" },
		"alter-budget":         func(m map[string]any) { m["max_completion_tokens"] = 65 },
		"null-becomes-absent":  func(m map[string]any) { delete(m, "temperature") },
		"null-becomes-zero":    func(m map[string]any) { m["temperature"] = 0 },
		"false-becomes-absent": func(m map[string]any) { delete(m, "parallel_tool_calls") },
		"empty-becomes-absent": func(m map[string]any) { delete(m, "stop") },
		"introduced-default":   func(m map[string]any) { m["seed"] = 1 },
		"tool-identity": func(m map[string]any) {
			m["messages"].([]any)[1].(map[string]any)["content"].([]any)[1].(map[string]any)["id"] = "call-2"
		},
		"integer-precision": func(m map[string]any) {
			m["messages"].([]any)[1].(map[string]any)["content"].([]any)[1].(map[string]any)["input"].(map[string]any)["n"] = json.Number("9007199254740992")
		},
		"schema-name":        func(m map[string]any) { delete(m["schema"].(map[string]any), "name") },
		"schema-description": func(m map[string]any) { delete(m["schema"].(map[string]any), "description") },
		"schema-strictness":  func(m map[string]any) { m["schema"].(map[string]any)["strict"] = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			v, err := fidelity.Decode(reference)
			if err != nil {
				t.Fatal(err)
			}
			m := v.(map[string]any)
			mutate(m)
			corrupted, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err = fidelity.Compare(reference, corrupted); err == nil {
				t.Fatal("semantic corruption passed")
			}
		})
	}
	for _, invalid := range []string{`{"x":1,"x":2}`, `{} {}`, `{"x":`, `[{]`} {
		if _, err := fidelity.Decode([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid or ambiguous JSON: %s", invalid)
		}
	}
	if err := fidelity.Compare([]byte(`{"a":1,"b":null}`), []byte(" { \"b\": null, \"a\": 1 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := fidelity.Compare([]byte(`{"secret":"opaque-fixture-signature-do-not-log"}`), []byte(`{"secret":{}}`)); err == nil || strings.Contains(err.Error(), "opaque-fixture") {
		t.Fatal("mismatch failed or exposed opaque state")
	}
}

func TestIndependentStreamOracleRequiresStateIdentityAndTerminal(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	if err = fidelity.CompareEvents(data, data); err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n")
	frames[0], frames[1] = frames[1], frames[0]
	mutations := map[string][]byte{
		"event-order":           []byte(strings.Join(frames, "\n\n") + "\n\n"),
		"terminal-deleted":      []byte(strings.Replace(string(data), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)),
		"late-state-altered":    []byte(strings.ReplaceAll(string(data), "opaque-fixture-signature-do-not-log", "corrupt-signature")),
		"tool-identity-altered": []byte(strings.ReplaceAll(string(data), "call-weather", "call-other")),
		"event-duplicated":      append(append([]byte{}, data...), []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")...),
		"unknown-event-drift":   []byte(strings.Replace(string(data), "thinking_delta", "unknown_native_delta", 1)),
	}
	for name, corrupted := range mutations {
		t.Run(name, func(t *testing.T) {
			if fidelity.CompareEvents(data, corrupted) == nil {
				t.Fatal("corrupt stream passed")
			}
		})
	}
}

func TestFrozenVersionOneReferences(t *testing.T) {
	manifest, err := fixtures.Files.ReadFile("v1/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	// This digest pins the fixture contracts and every referenced artifact together.
	const manifestDigest = "c71cbc5a59e44059e1c0b2f53365f6acd91821a9bf1ced7ed731d704db60e878"
	actual := sha256.Sum256(manifest)
	if hex.EncodeToString(actual[:]) != manifestDigest {
		t.Fatal("v1 references changed: add a new version instead of rewriting the reference")
	}
	var files struct {
		Files map[string]string `json:"files"`
	}
	if err = json.Unmarshal(manifest, &files); err != nil {
		t.Fatal(err)
	}
	entries, err := fixtures.Files.ReadDir("v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		if _, listed := files.Files[entry.Name()]; !listed {
			t.Fatal("unlisted v1 reference", entry.Name())
		}
	}
	for name, digest := range files.Files {
		data, err := fixtures.Files.ReadFile("v1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != digest {
			t.Errorf("frozen reference changed: %s", name)
		}
	}
	data, err := fixtures.Files.ReadFile("v1/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Rows []struct{ ID, Operation, Profile, Dialect, Mode, Client string }
	}
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range inventory.Rows {
		if row.ID == "" || row.Operation == "" || row.Profile == "" || row.Dialect == "" || row.Mode == "" || row.Client == "" || seen[row.ID] {
			t.Fatal("invalid or repeated inventory row", row.ID)
		}
		seen[row.ID] = true
	}
	if len(seen) != 47 {
		t.Fatal(fmt.Sprintf("denominator changed: %d, expected 47", len(seen)))
	}
}

func TestOracleRejectsUnicodeRepairAndPrecisionLoss(t *testing.T) {
	for _, corrupted := range [][]byte{
		[]byte(`{"opaque":"\ud800"}`),
		[]byte(`{"opaque":"\udc00"}`),
		[]byte(`{"opaque":"\ud800\u0041"}`),
		append(append([]byte(`{"opaque":"`), 0xff), []byte(`"}`)...),
	} {
		if fidelity.Compare([]byte(`{"opaque":"�"}`), corrupted) == nil {
			t.Fatal("Unicode repair hid corruption")
		}
		if _, err := fidelity.Decode(corrupted); err == nil {
			t.Fatal("ambiguous Unicode accepted")
		}
	}
	for _, valid := range []string{`{"opaque":"\ud83d\ude00"}`, `{"opaque":"\\ud800"}`, `{"opaque":"�"}`} {
		if _, err := fidelity.Decode([]byte(valid)); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`},
		{`{"n":1.0000000000000001}`, `{"n":1.0000000000000002}`},
		{`{"n":1e400}`, `{"n":1e399}`},
		{`{"opaque":"é"}`, `{"opaque":"é"}`},
	} {
		if fidelity.Compare([]byte(pair[0]), []byte(pair[1])) == nil {
			t.Fatal("required precision or opaque representation lost")
		}
	}
}

func FuzzReferenceRetainsOpaqueState(f *testing.F) {
	for _, seed := range []string{"signature", "", "é", "\u0000", "😀", "9007199254740993"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		original, _ := json.Marshal(map[string]any{"state": value, "ordered": []int{0, 1, 2}})
		if err := fidelity.Compare(original, original); err != nil {
			t.Fatal(err)
		}
		changed, _ := json.Marshal(map[string]any{"state": value + "!", "ordered": []int{0, 1, 2}})
		if fidelity.Compare(original, changed) == nil {
			t.Fatal("changed opaque state passed")
		}
		reordered, _ := json.Marshal(map[string]any{"state": value, "ordered": []int{0, 2, 1}})
		if fidelity.Compare(original, reordered) == nil {
			t.Fatal("changed order passed")
		}
	})
}

func TestIndependentNextRequestReferenceRejectsMissingNativeDependencies(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, corruption := range []string{"missing-reasoning", "altered-signature", "changed-tool-result", "removed-history"} {
		t.Run(corruption, func(t *testing.T) {
			value, err := fidelity.Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			request := value.(map[string]any)
			history := request["messages"].([]any)
			assistant := history[1].(map[string]any)
			content := assistant["content"].([]any)
			switch corruption {
			case "missing-reasoning":
				assistant["content"] = content[1:]
			case "altered-signature":
				content[0].(map[string]any)["signature"] = "other-signature"
			case "changed-tool-result":
				history[2].(map[string]any)["content"].([]any)[0].(map[string]any)["tool_use_id"] = "call-clock"
			case "removed-history":
				request["messages"] = history[1:]
			}
			observed, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if fidelity.Compare(data, observed) == nil {
				t.Fatal("native dependency corruption passed")
			}
		})
	}
}

func TestIndependentStreamOraclePreservesEventIDState(t *testing.T) {
	reset := []byte("id: persisted\ndata: {\"part\":1}\n\nid:\ndata: {\"part\":2}\n\n")
	missingReset := []byte("id: persisted\ndata: {\"part\":1}\n\ndata: {\"part\":2}\n\n")
	if fidelity.CompareEvents(reset, missingReset) == nil {
		t.Fatal("deleted event ID reset changed client continuation identity unnoticed")
	}
	explicitInherited := []byte("id: persisted\ndata: {\"part\":1}\n\nid: persisted\ndata: {\"part\":2}\n\n")
	if err := fidelity.CompareEvents(explicitInherited, missingReset); err != nil {
		t.Fatal("SSE event ID inheritance differs", err)
	}
	idOnly := []byte("id: persisted\n\ndata: {\"part\":1}\n\nid:\ndata: {\"part\":2}\n\n")
	if err := fidelity.CompareEvents(reset, idOnly); err != nil {
		t.Fatal("ID-only frame did not update event identity", err)
	}
}
