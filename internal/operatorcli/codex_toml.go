package operatorcli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

const codexOLPProvider = 2

var codexKeys = [...]string{"model", "model_provider", "model_providers.olp"}

type codexSpan struct {
	start, end int
}

type codexDocument struct {
	text     []byte
	fields   [len(codexKeys)]codexSpan
	provider map[string]any
	values   [2]codexSpan
	journal  codexSpan
	model    string
	selected string
}

func (doc codexDocument) field(index int) string {
	span := doc.fields[index]
	return string(doc.text[span.start:span.end])
}

func parseCodexTOML(text []byte) (codexDocument, error) {
	var decoded map[string]any
	if err := toml.Unmarshal(text, &decoded); err != nil {
		return codexDocument{}, errors.New("invalid Codex TOML; fix the syntax or duplicate keys before continuing")
	}
	doc := codexDocument{text: text}
	parser := unstable.Parser{KeepComments: true}
	parser.Reset(text)
	var section []string
	for parser.NextExpression() {
		node := parser.Expression()
		if node.Kind == unstable.Comment {
			if bytes.HasPrefix(node.Data, []byte(codexJournalPrefix)) {
				if doc.journal.end != 0 {
					return codexDocument{}, errors.New("multiple OLP setup records in Codex config")
				}
				doc.journal = codexSpan{int(node.Raw.Offset), int(node.Raw.Offset + node.Raw.Length)}
			}
			continue
		}
		key := node.Key()
		parts := slices.Collect(func(yield func(string) bool) {
			for key.Next() && yield(string(key.Node().Data)) {
			}
		})
		switch node.Kind {
		case unstable.Table, unstable.ArrayTable:
			section = parts
			olp := len(section) >= 2 && section[0] == "model_providers" && section[1] == "olp"
			if olp && (node.Kind != unstable.Table || len(section) != 2) {
				return codexDocument{}, errors.New("unsupported Codex config shape for model_providers.olp; expected a single table")
			}
			if olp {
				first := node.Key()
				first.Next()
				offset := int(first.Node().Raw.Offset)
				doc.fields[codexOLPProvider] = codexSpan{codexLineStart(text, offset), codexLineEnd(text, offset)}
			}
		case unstable.KeyValue:
			if len(section) == 0 && len(parts) == 1 {
				index := slices.Index(codexKeys[:2], parts[0])
				if index >= 0 && node.Value().Kind == unstable.String {
					raw := node.Value().Raw
					doc.values[index] = codexSpan{int(raw.Offset), int(raw.Offset + raw.Length)}
					doc.fields[index] = codexSpan{codexLineStart(text, int(raw.Offset)), codexLineEnd(text, int(raw.Offset+raw.Length))}
				}
			}
			if slices.Equal(section, []string{"model_providers", "olp"}) && doc.field(codexOLPProvider) != "" {
				first := node.Key()
				first.Next()
				doc.fields[codexOLPProvider].end = codexLineEnd(text, max(int(first.Node().Raw.Offset), codexValueEnd(node.Value())))
			}
		}
	}
	if err := parser.Error(); err != nil {
		return codexDocument{}, errors.New("could not locate settings in Codex TOML")
	}
	for index, key := range codexKeys[:2] {
		if _, present := decoded[key]; present && doc.field(index) == "" {
			return codexDocument{}, fmt.Errorf("unsupported Codex config shape for %s; expected a top-level string", key)
		}
	}
	if providers, present := decoded["model_providers"]; present {
		table, ok := providers.(map[string]any)
		if !ok {
			return codexDocument{}, errors.New("unsupported Codex config shape for model_providers; expected a table")
		}
		if provider, present := table["olp"]; present {
			settings, ok := provider.(map[string]any)
			if !ok || doc.field(codexOLPProvider) == "" {
				return codexDocument{}, errors.New("unsupported Codex config shape for model_providers.olp; expected a dedicated table")
			}
			doc.provider = settings
		}
	}
	doc.model, _ = decoded["model"].(string)
	doc.selected, _ = decoded["model_provider"].(string)
	return doc, nil
}

func codexValueEnd(value *unstable.Node) int {
	offset := int(value.Raw.Offset + value.Raw.Length)
	for child := value.Children(); child.Next(); {
		offset = max(offset, codexValueEnd(child.Node()))
	}
	return offset
}

func codexLineStart(text []byte, offset int) int {
	return bytes.LastIndexByte(text[:offset], '\n') + 1
}

func codexLineEnd(text []byte, offset int) int {
	newline := bytes.IndexByte(text[offset:], '\n')
	if newline < 0 {
		return len(text)
	}
	end := offset + newline
	if end > 0 && text[end-1] == '\r' {
		return end - 1
	}
	return end
}

func (doc codexDocument) checkProvider() error {
	for _, key := range slices.Sorted(maps.Keys(doc.provider)) {
		value := doc.provider[key]
		switch key {
		case "name", "env_key", "wire_api":
			if _, ok := value.(string); ok {
				continue
			}
		case "base_url":
			if endpoint, ok := value.(string); ok && safeCodexEndpoint(endpoint) {
				continue
			}
		case "requires_openai_auth", "supports_websockets":
			if _, ok := value.(bool); ok {
				continue
			}
		case "http_headers":
			if headers, ok := value.(map[string]any); ok && len(headers) == 1 {
				if _, ok := headers["X-OLP-Code-Model"].(string); ok {
					continue
				}
			}
		}
		return errors.New("unsupported settings in model_providers.olp; move the custom provider to another name before applying")
	}
	return nil
}

func safeCodexEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(endpoint, "#")
}

type codexEdit struct {
	codexSpan
	text string
}

func (doc codexDocument) edit(replacements [len(codexKeys)]string, journal string, joined bool) []byte {
	newline := codexNewline(doc.text)
	edits := make([]codexEdit, 0, len(codexKeys)+1)
	if doc.journal.end != 0 {
		if journal == "" {
			edits = append(edits, codexEdit{codexSpan: doc.wholeLine(doc.journal)})
		} else {
			edits = append(edits, codexEdit{doc.journal, journal})
		}
	} else if journal != "" {
		edits = append(edits, codexEdit{text: journal + newline})
	}
	for index, replacement := range replacements {
		edits = append(edits, doc.settingEdit(index, replacement, newline, joined, edits))
	}
	slices.SortStableFunc(edits, func(a, b codexEdit) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end))
	})
	var out bytes.Buffer
	offset := 0
	for _, edit := range edits {
		out.Write(doc.text[offset:edit.start])
		out.WriteString(edit.text)
		offset = edit.end
	}
	out.Write(doc.text[offset:])
	return out.Bytes()
}

func (doc codexDocument) settingEdit(index int, replacement, newline string, joined bool, prior []codexEdit) codexEdit {
	span := doc.fields[index]
	if span.start == span.end {
		if replacement == "" {
			return codexEdit{}
		}
		if index != codexOLPProvider {
			return codexEdit{text: replacement + newline}
		}
		end := codexSpan{len(doc.text), len(doc.text)}
		if joined {
			return codexEdit{end, newline + replacement + newline}
		}
		return codexEdit{end, replacement + newline}
	}
	if replacement != "" {
		return codexEdit{span, replacement}
	}
	line := doc.wholeLine(span)
	if index != codexOLPProvider || !joined || line.end != len(doc.text) || line.start == 0 || doc.text[line.start-1] != '\n' {
		return codexEdit{codexSpan: line}
	}
	start := codexLineEnd(doc.text, line.start-1)
	if slices.ContainsFunc(prior, func(edit codexEdit) bool { return edit.end > start }) {
		return codexEdit{codexSpan: line}
	}
	return codexEdit{codexSpan: codexSpan{start, line.end}}
}

func codexNewline(text []byte) string {
	if first := bytes.IndexByte(text, '\n'); first > 0 && text[first-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

func (doc codexDocument) wholeLine(span codexSpan) codexSpan {
	start := codexLineStart(doc.text, span.start)
	if after := bytes.IndexByte(doc.text[span.end:], '\n'); after >= 0 {
		return codexSpan{start, span.end + after + 1}
	}
	return codexSpan{start, span.end}
}
