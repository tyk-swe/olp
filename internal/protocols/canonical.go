package protocols

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type Part struct{ Text, URL, MIME, Detail string }
type Message struct {
	Role, Name, ToolID string
	Parts              []Part
	Calls              []openai.ToolCall
}
type Tool struct {
	Name, Description string
	Schema            json.RawMessage
}
type Generation struct {
	Messages   []Message
	Tools      []Tool
	Parameters Object
	Extensions []string
	Stream     bool
}

// reader consumes only represented semantics. Everything left over is a source
// extension, including unknown nested content and tool fields.
type reader struct {
	f    Object
	path string
	c    *Generation
}

func read(v json.RawMessage, path string, c *Generation) (*reader, error) {
	f, e := object(v)
	return &reader{f, path, c}, e
}
func (r *reader) take(k string) json.RawMessage { v := r.f[k]; delete(r.f, k); return v }
func (r *reader) finish() {
	for k, v := range r.f {
		if present(v) {
			r.c.Extensions = append(r.c.Extensions, r.path+"/"+k)
		}
	}
}
func (r *reader) parameter(from, to string) {
	if v := r.take(from); present(v) {
		r.c.Parameters[to] = v
	}
}

func decodeCanonical(family openai.Family, fields Object) (*Generation, error) {
	c := &Generation{Parameters: Object{}}
	f := Object{}
	maps.Copy(f, fields)
	r := &reader{f: f, c: c}
	r.take("model")
	r.take("stream")
	r.take("stream_options")
	var err error
	switch family.Surface() {
	case "openai":
		err = decodeOpenAIGeneration(r, family)
	case "anthropic":
		err = decodeAnthropicGeneration(r)
	default:
		err = decodeGeminiGeneration(r)
	}
	if err != nil {
		return nil, err
	}
	r.finish()
	return r.c, nil
}

func decodeTool(t *reader) Tool {
	return Tool{Name: str(t.take("name")), Description: str(t.take("description")), Schema: t.take("parameters")}
}
func decodeParts(v json.RawMessage, family, path string, c *Generation) ([]Part, error) {
	if !present(v) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(v, &text) == nil {
		return []Part{{Text: text}}, nil
	}
	values := arr(v)
	if values == nil {
		return nil, unsupported(path)
	}
	parts := []Part{}
	for i, v := range values {
		p := fmt.Sprintf("%s/%d", path, i)
		r, e := read(v, p, c)
		if e != nil {
			return nil, e
		}
		kind := str(r.take("type"))
		part := Part{}
		if family == "gemini" {
			if text := r.take("text"); present(text) {
				part.Text = str(text)
			} else if data := r.take("inlineData"); present(data) {
				s, e := read(data, p+"/inlineData", c)
				if e != nil {
					return nil, e
				}
				part.MIME = str(s.take("mimeType"))
				part.URL = "data:" + part.MIME + ";base64," + str(s.take("data"))
				s.finish()
			} else if data := r.take("fileData"); present(data) {
				s, e := read(data, p+"/fileData", c)
				if e != nil {
					return nil, e
				}
				part.MIME = str(s.take("mimeType"))
				part.URL = str(s.take("fileUri"))
				s.finish()
			} else {
				c.Extensions = append(c.Extensions, p)
			}
		} else {
			switch kind {
			case "text", "input_text", "output_text":
				part.Text = str(r.take("text"))
			case "image_url":
				s, e := read(r.take("image_url"), p+"/image_url", c)
				if e != nil {
					return nil, e
				}
				part.URL = str(s.take("url"))
				part.Detail = str(s.take("detail"))
				s.finish()
			case "input_image":
				part.URL = str(r.take("image_url"))
				part.Detail = str(r.take("detail"))
			case "image":
				s, e := read(r.take("source"), p+"/source", c)
				if e != nil {
					return nil, e
				}
				switch str(s.take("type")) {
				case "base64":
					part.MIME = str(s.take("media_type"))
					part.URL = "data:" + part.MIME + ";base64," + str(s.take("data"))
				case "url":
					part.URL = str(s.take("url"))
				default:
					c.Extensions = append(c.Extensions, p+"/source/type")
				}
				s.finish()
			default:
				c.Extensions = append(c.Extensions, p+"/type")
			}
		}
		if part.MIME == "" && strings.HasPrefix(part.URL, "data:") {
			part.MIME, _, _ = strings.Cut(strings.TrimPrefix(part.URL, "data:"), ";")
		}
		parts = append(parts, part)
		r.finish()
	}
	return parts, nil
}

func encodeCanonical(c *Generation, family openai.Family, model, operation string) (Object, error) {
	count := operation == "token_count"
	switch family {
	case "bedrock", "bedrock_count":
		return encodeBedrock(c, model, operation)
	case openai.FamilyChat, openai.FamilyInputTokens:
		return encodeOpenAIGeneration(c, model, count)
	case openai.FamilyAnthropic, openai.FamilyAnthropicCount:
		return encodeAnthropicGeneration(c, model, count)
	case openai.FamilyGemini, openai.FamilyGeminiCount:
		return encodeGeminiGeneration(c, model, count)
	default:
		return nil, unsupported("operation")
	}
}

func joinText(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
func encodeParts(parts []Part, family string) ([]Object, error) {
	out := []Object{}
	for _, p := range parts {
		if p.Detail != "" {
			return nil, unsupported("image detail")
		}
		if p.URL == "" {
			v := Object{"text": raw(p.Text)}
			if family == "anthropic" {
				v["type"] = raw("text")
			}
			out = append(out, v)
			continue
		}
		if family == "anthropic" {
			source := Object{"type": raw("url"), "url": raw(p.URL)}
			if strings.HasPrefix(p.URL, "data:") {
				_, data, ok := strings.Cut(p.URL, ";base64,")
				if !ok || p.MIME == "" {
					return nil, unsupported("image data")
				}
				source = Object{"type": raw("base64"), "media_type": raw(p.MIME), "data": raw(data)}
			}
			out = append(out, Object{"type": raw("image"), "source": raw(source)})
		} else {
			if p.MIME == "" {
				return nil, unsupported("image MIME type")
			}
			if strings.HasPrefix(p.URL, "data:") {
				_, data, ok := strings.Cut(p.URL, ";base64,")
				if !ok {
					return nil, unsupported("image data")
				}
				out = append(out, Object{"inlineData": raw(map[string]string{"mimeType": p.MIME, "data": data})})
			} else {
				out = append(out, Object{"fileData": raw(map[string]string{"mimeType": p.MIME, "fileUri": p.URL})})
			}
		}
	}
	return out, nil
}
