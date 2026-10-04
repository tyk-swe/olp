package estimate

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

const (
	// ImageTokens and MediaTokens are the flat charges for content whose size on
	// the wire says nothing about what a model will be billed for: an inline
	// image arrives as a megabyte of base64, and charging it by its length would
	// refuse requests no provider would have refused.
	ImageTokens = 1000
	MediaTokens = 2000
	// DefaultOutputTokens stands in for a caller that named no output bound.
	DefaultOutputTokens = 4096
	// MaxTokens is the largest integer the limiter can store. Every estimate
	// saturates here, so an absurd request is rejected as too large for a key's
	// window rather than failing the reservation as malformed.
	MaxTokens = 1<<53 - 1
)

// retainBytes is how much of a prompt's text is kept. A Meter reads the first
// ExactBytes of the text and a scanWindow past them and nothing else of it, so
// that is all a prompt keeps, with room for the character a cut may have to
// back up over, and the rest of a request's text, which may be megabytes of it
// and is held in flight for as long as the request, is counted and let go.
const retainBytes = ExactBytes + 2*scanWindow

// segment is one text the tokenizer counts on its own, with the heuristic
// charge measured when it was read.
type segment struct {
	// text is the segment, or its first bytes when the prompt ran out of room
	// for it, and rest is how many bytes of the segment are not kept. A prompt
	// that has kept all it will keep gathers every later segment into one with
	// no text, whose rest is their sum.
	text string
	rest int64
	// heuristic is the charge for the whole segment, rest included.
	heuristic int64
}

// input is what walking a request's prompt found: the text a counter counts,
// and the charges that do not depend on the counter.
type input struct {
	text []segment
	// kept is the bytes of text the segments hold, and bytes the bytes of text
	// the walk read. retain is how many bytes may be kept, retainBytes unless a
	// test says otherwise.
	kept, retain int
	bytes        int64
	// heuristic is the sum of the heuristic charges of text, which is what a
	// counter without a tokenizer reports before its factor.
	heuristic int64
	// roles are the roles of the request's messages. They are text only a
	// tokenizer counts, as the OpenAI Cookbook formula does; the heuristic has
	// never charged them.
	roles []string
	// messages and names are what a tokenizer's framing is charged on.
	messages, names int
	// flat is charged the same whatever counts the request: images and media
	// parts at their flat rates, and token ids at one each.
	flat int64
	// approx says the prompt holds something a tokenizer cannot count as the
	// provider does, which makes even an exact count of its text a guess about
	// the request: an image or media part charged at a flat rate, and a tool
	// schema or a tool call, which a model reads in a rendering no provider
	// documents.
	approx bool
}

// addText adds one text. A text that is part of a request's document is a view
// of it, and a segment that keeps a view would hold the document up, so what is
// kept of one is copied.
func (in *input) addText(text string, view bool) {
	if text == "" {
		return
	}
	charge := HeuristicTokens(text)
	in.heuristic = addBounded(in.heuristic, charge)
	in.bytes += int64(len(text))
	retain := retainBytes
	if in.retain > 0 {
		retain = in.retain
	}
	room := retain - in.kept
	if room >= len(text) {
		if view {
			text = strings.Clone(text)
		}
		in.text = append(in.text, segment{text: text, heuristic: charge})
		in.kept += len(text)
		return
	}
	if room > 0 {
		// The prefix is copied so that it does not hold the whole text up.
		prefix := strings.Clone(prefixOf(text, room))
		in.text = append(in.text, segment{text: prefix, rest: int64(len(text) - len(prefix)), heuristic: charge})
		in.kept = retain
		return
	}
	if n := len(in.text); n > 0 && in.text[n-1].text == "" {
		last := &in.text[n-1]
		last.rest += int64(len(text))
		last.heuristic = addBounded(last.heuristic, charge)
		return
	}
	in.text = append(in.text, segment{rest: int64(len(text)), heuristic: charge})
}

func (in *input) addFlat(tokens int64) { in.flat = addBounded(in.flat, tokens) }

func (in *input) addMedia(tokens int64) {
	in.addFlat(tokens)
	in.approx = true
}

// empty reports whether the walk found nothing to charge.
func (in *input) empty() bool {
	return len(in.text) == 0 && len(in.roles) == 0 && in.messages == 0 && in.flat == 0
}

// equal reports whether any counter would count the two the same.
func (in *input) equal(other *input) bool {
	if in.flat != other.flat || in.approx != other.approx || in.messages != other.messages || in.names != other.names ||
		in.bytes != other.bytes || len(in.text) != len(other.text) || len(in.roles) != len(other.roles) {
		return false
	}
	if slices.Equal(in.text, other.text) && slices.Equal(in.roles, other.roles) {
		return true
	}
	// Where all of the text is counted exactly, which is where a Meter has room
	// for all of it and roles included, a count is a sum over its segments and
	// does not depend on their order: the request a provider is sent often holds
	// what the caller's did, with the system prompt in another place. Past that
	// the count is extrapolated from what came first, and order is part of it.
	if in.bytes+roleBytes(in.roles) > ExactBytes {
		return false
	}
	return sameElements(in.text, other.text, func(a, b segment) int { return strings.Compare(a.text, b.text) }) &&
		sameElements(in.roles, other.roles, strings.Compare)
}

// sameElements reports whether a and b hold the same elements, in any order.
func sameElements[T comparable](a, b []T, compare func(T, T) int) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, compare)
	slices.SortFunc(b, compare)
	return slices.Equal(a, b)
}

func roleBytes(roles []string) int64 {
	var n int64
	for _, role := range roles {
		n += int64(len(role))
	}
	return n
}

// walker reads the prompt of one request into an input. It is the one place
// that knows which fields of which dialect are prompt: text at four characters
// per token, a flat charge per media part, and nothing for a shape it does not
// recognise, because a reservation is reconciled against the usage the
// upstream reports as soon as the attempt ends.
type walker struct {
	in *input
}

// promptFields are the fields of a request that walkInput reads, in any
// dialect.
var promptFields = []string{
	"messages", "input", "instructions", "system", "contents", "systemInstruction", "generateContentRequest", "tools",
	"toolConfig", "response_format", "text", "output_config", "output_format", "generationConfig", "outputConfig",
}

// promptMembers are the members the walker reads of the fields of promptFields
// that it reads only part of. An object that names none of them has no prompt
// text, whatever else it sets.
var promptMembers = map[string][]string{
	"generationConfig": responseSchemas,
	"response_format":  {"json_schema"},
	"text":             {"format"},
	"output_config":    {"format"},
	"output_format":    {"schema"},
	"outputConfig":     {"textFormat"},
}

// responseSchemas are the members of a Gemini generation config that carry the
// schema a reply must follow.
var responseSchemas = []string{"responseSchema", "responseJsonSchema"}

// requestFields reads the top-level fields of a request, asking its document for
// each one by name.
func requestFields(request *openai.Request) func(string) node {
	root := request.OIF().Document().Root()
	return func(name string) node {
		v, _ := root.Lookup(name)
		return valueNode(v)
	}
}

// walkInput reads the prompt fields of a request of the given family, asking
// field for each one by name.
func walkInput(in *input, family openai.Family, field func(string) node) {
	w := walker{in}
	switch family {
	case openai.FamilyChat:
		w.items(field("messages"))
	case openai.FamilyEmbeddings:
		w.embedding(field("input"))
	case openai.FamilyResponses, openai.FamilyInputTokens, openai.FamilyModeration:
		w.items(field("input"))
		w.instructions(field("instructions"))
	}
	if family.Surface() != "openai" {
		w.dialectMessages(field("messages"))
		w.dialectSystem(field("system"))
		w.dialectMessages(field("contents"))
		w.dialectSystem(field("systemInstruction"))
		w.dialectRequest(field("generateContentRequest"))
		w.schema(field("tools"))
		if family.Surface() == "bedrock" {
			// Converse holds its tool catalogue beside the conversation, where
			// the other dialects hold it in tools; Gemini's toolConfig only
			// chooses among the tools it already holds.
			w.schema(field("toolConfig"))
		}
		w.structured(family, field)
		return
	}
	w.tools(field("tools"))
	w.structured(family, field)
}

// structured walks the schema a request asks the reply to follow: a model reads
// it as prompt text, as it reads a tool's. Each dialect has its own place for
// one, and a request without one costs nothing to look.
func (w walker) structured(family openai.Family, field func(string) node) {
	switch family {
	case openai.FamilyChat:
		w.schema(field("response_format").object().get("json_schema").object().get("schema"))
	case openai.FamilyResponses, openai.FamilyInputTokens:
		w.schema(field("text").object().get("format").object().get("schema"))
	}
	switch family.Surface() {
	case "anthropic":
		w.schema(field("output_config").object().get("format").object().get("schema"))
		w.schema(field("output_format").object().get("schema"))
	case "gemini":
		w.generationConfig(field("generationConfig"))
	case "bedrock":
		// Converse names the schema as a string of JSON, which is its text.
		schema := field("outputConfig").object().get("textFormat").object().get("structure").object().get("jsonSchema").object().get("schema")
		if !w.toolText(schema) {
			w.schema(schema)
		}
	}
}

// generationConfig walks the response schema of a Gemini request, in either of
// the fields that carry one.
func (w walker) generationConfig(n node) {
	config := n.object()
	for _, member := range responseSchemas {
		w.schema(config.get(member))
	}
}

// text adds a JSON string as one segment, reporting whether the value was a
// string. Characters are counted rather than bytes so a multi-byte script is
// not overcharged.
func (w walker) text(n node) bool {
	text, view, ok := n.text()
	if ok {
		w.in.addText(text, view)
	}
	return ok
}

// toolText adds text that belongs to a tool call or its result. It counts as
// text, but the model reads it inside a rendering of the call that no provider
// documents, which the count does not include.
func (w walker) toolText(n node) bool {
	ok := w.text(n)
	if ok {
		w.in.approx = true
	}
	return ok
}

// items walks the conversation one request carries: the messages of a chat
// request, or the input of a responses request, which may also be one plain
// string. A shape this gateway does not recognise is charged nothing rather
// than guessed at.
func (w walker) items(n node) {
	if w.text(n) {
		// A plain string is what a caller who sent no messages means by one.
		w.in.messages++
		w.in.roles = append(w.in.roles, "user")
		return
	}
	for _, n := range n.elements() {
		item := n.object()
		if !item.exists() {
			continue
		}
		w.message(item)
		if item.get("type").source() == `"reasoning"` {
			w.reasoning(item)
		}
		// A responses tool result carries what it returned in `output`.
		w.content(item.get("content"))
		w.content(item.get("output"))
		// The identifiers and arguments a message travels with are prompt text
		// like any other; `call_id` and `arguments` are the responses spelling
		// of the chat tool-call fields.
		w.text(item.get("name"))
		for _, name := range [...]string{"tool_call_id", "call_id", "arguments"} {
			w.toolText(item.get(name))
		}
		for _, n := range item.get("tool_calls").elements() {
			call := n.object()
			if function := call.get("function").object(); function.exists() {
				call = function
			}
			w.toolText(call.get("name"))
			w.toolText(call.get("arguments"))
		}
	}
}

// reasoning walks what a reasoning item of a responses conversation says that a
// model reads again: the summary it wrote. Its reasoning text is a content part,
// read with the item's content. What else it carries, the encrypted content, is
// the rest of the reasoning in a form no count reads, and is charged nothing, so
// the count is a guess.
func (w walker) reasoning(item object) {
	for _, n := range item.get("summary").elements() {
		w.toolText(n.object().get("text"))
	}
	w.in.approx = true
}

// message records the framing of one message: it exists, it has a role, and
// possibly a name.
func (w walker) message(item object) {
	w.in.messages++
	if role, ok := roleOf(item.get("role")); ok {
		w.in.roles = append(w.in.roles, role)
	}
	if _, _, ok := item.get("name").text(); ok {
		w.in.names++
	}
}

// roleOf reads a message role. The usual ones are answered without decoding,
// since a request carries a role for every message.
func roleOf(n node) (string, bool) {
	switch n.source() {
	case `"user"`:
		return "user", true
	case `"assistant"`:
		return "assistant", true
	case `"system"`:
		return "system", true
	case `"tool"`:
		return "tool", true
	case `"developer"`:
		return "developer", true
	}
	text, _, ok := n.text()
	return text, ok
}

// instructions walks the instructions of a responses request, which a model
// reads as a message of their own.
func (w walker) instructions(n node) {
	if text, view, ok := n.text(); ok && text != "" {
		w.in.addText(text, view)
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// content walks one message body: plain text, or the parts a multimodal
// message carries.
func (w walker) content(n node) {
	if w.text(n) {
		return
	}
	for _, n := range n.elements() {
		part := n.object()
		if !part.exists() {
			// A bare string among the parts is text.
			w.text(n)
			continue
		}
		kind, _, _ := part.get("type").text()
		switch kind {
		case "image_url", "input_image":
			w.in.addMedia(ImageTokens)
		case "input_audio", "input_file", "file":
			w.in.addMedia(MediaTokens)
		default:
			w.text(part.get("text"))
			w.text(part.get("refusal"))
		}
	}
}

// tools walks the tool catalogue a request carries: a schema the model has to
// read costs what any other prompt text costs.
func (w walker) tools(n node) {
	for _, n := range n.elements() {
		tool := n.object()
		// Chat nests the definition under `function`; responses holds it flat.
		if function := tool.get("function").object(); function.exists() {
			tool = function
		}
		w.toolText(tool.get("name"))
		w.toolText(tool.get("description"))
		w.schema(tool.get("parameters"))
	}
}

// schema adds a JSON document as the text it is, with whatever formatting the
// caller happened to send removed.
func (w walker) schema(n node) {
	if text := SchemaText(n.bytes()); text != "" {
		w.in.addText(text, false)
		w.in.approx = true
	}
}

// SchemaText is a JSON document as the text a model reads, with whatever
// formatting the caller happened to send removed. It is empty for no document
// and for one that is not JSON.
func SchemaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return ""
	}
	return compact.String()
}

// embedding walks the input of an embeddings request: texts, or the token ids
// a caller already has, which are one token each.
func (w walker) embedding(n node) {
	if w.text(n) {
		return
	}
	for _, item := range n.elements() {
		var token uint32
		if json.Unmarshal(item.bytes(), &token) == nil {
			w.in.addFlat(1)
		} else {
			w.embedding(item)
		}
	}
}

// dialectMessages walks the conversation of a native Anthropic, Gemini or
// Bedrock request, whose entries are messages in a shape of their own.
func (w walker) dialectMessages(n node) {
	w.dialect(n)
	for _, n := range n.elements() {
		if item := n.object(); item.exists() {
			w.in.messages++
			if role, ok := roleOf(item.get("role")); ok {
				w.in.roles = append(w.in.roles, role)
			}
		}
	}
}

// dialectSystem walks a system prompt, which a model reads as a message.
func (w walker) dialectSystem(n node) {
	w.dialect(n)
	if n.present() && !n.isNull() {
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// dialectRequest walks the request a Gemini count wraps.
func (w walker) dialectRequest(n node) {
	w.dialect(n)
	inner := n.object()
	w.schema(inner.get("tools"))
	w.generationConfig(inner.get("generationConfig"))
	for _, n := range inner.get("contents").elements() {
		if item := n.object(); item.exists() {
			w.in.messages++
			if role, ok := roleOf(item.get("role")); ok {
				w.in.roles = append(w.in.roles, role)
			}
		}
	}
	if n := inner.get("systemInstruction"); n.present() && !n.isNull() {
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// dialect charges native content the same text and media units as OpenAI's.
// Blob bytes are never mistaken for text tokens.
func (w walker) dialect(n node) {
	if !n.present() || w.text(n) {
		return
	}
	if items, ok := n.array(); ok {
		for _, item := range items {
			w.dialect(item)
		}
		return
	}
	f := n.object()
	if !f.exists() {
		return
	}
	if f.get("inlineData").present() || f.get("fileData").present() || f.get("type").source() == `"image"` {
		w.in.addMedia(ImageTokens)
		return
	}
	switch f.get("type").source() {
	case `"thinking"`:
		// The text the model reasoned in, which it reads again in a tool loop.
		w.toolText(f.get("thinking"))
		w.in.approx = true
		return
	case `"redacted_thinking"`:
		// Its reasoning, encrypted: nothing a count can read.
		w.in.approx = true
		return
	case `"document"`:
		w.document(f)
		return
	}
	// A tool call or its result, whichever keys carry its text.
	if kind := f.get("type").source(); kind == `"tool_use"` || kind == `"tool_result"` {
		w.in.approx = true
	}
	for _, key := range [...]string{"text", "content", "parts", "contents", "systemInstruction", "functionResponse", "functionCall", "input", "output"} {
		if value := f.get(key); value.present() {
			switch key {
			case "functionResponse", "functionCall", "input", "output":
				w.in.approx = true
			}
			w.dialect(value)
		}
	}
}

// document walks an Anthropic document block: the text of one given as text, or
// as content blocks, and for any other source, a PDF or a file as base64 or a
// URL, the flat charge of a media part, whatever the bytes it carries.
func (w walker) document(block object) {
	source := block.get("source").object()
	switch source.get("type").source() {
	case `"text"`:
		w.toolText(source.get("data"))
	case `"content"`:
		w.dialect(source.get("content"))
	default:
		w.in.addMedia(MediaTokens)
		return
	}
	w.in.approx = true
}

// OutputBounds reads how much reply a request allows: the bound it named for
// one candidate, and how many candidates it asked for. Defaults supply omitted
// top-level fields. For an effective provider bound, pass the destination
// request with its defaults already merged and nil defaults.
func OutputBounds(request *openai.Request, defaults map[string]json.RawMessage) (output *int64, candidates int64) {
	field := func(name string) json.RawMessage { return fieldOf(request, defaults, name) }
	candidates = 1
	outputFields := []string{"max_completion_tokens", "max_tokens"}
	if request != nil && request.Family == openai.FamilyResponses {
		outputFields = []string{"max_output_tokens"}
	}
	for _, name := range outputFields {
		if value, ok := integerValue(field(name)); ok {
			output = &value
			break
		}
	}
	if request != nil && request.Family == openai.FamilyBedrock {
		if value, ok := integerValue(jsonObject(field("inferenceConfig"))["maxTokens"]); ok {
			output = &value
		}
	}
	if request != nil && request.Family.Surface() == "gemini" {
		config := jsonObject(field("generationConfig"))
		if v, ok := integerValue(config["maxOutputTokens"]); ok {
			output = &v
		}
		if v, ok := integerValue(config["candidateCount"]); ok {
			candidates = v
		}
	}
	if request == nil || request.Family == openai.FamilyChat {
		if value, ok := integerValue(field("n")); ok {
			candidates = value
		}
	}
	return output, candidates
}

// fieldOf reads a field the way the encoder resolves it: the caller's value,
// then the provider's default. Match Encode's precedence, including an explicit
// null opting out of a default and either chat token-bound alias overriding the
// other.
func fieldOf(request *openai.Request, defaults map[string]json.RawMessage, name string) json.RawMessage {
	if request != nil {
		if raw := request.Field(name); len(raw) > 0 {
			return raw
		}
		if request.Family == openai.FamilyChat &&
			((name == "max_tokens" && len(request.Field("max_completion_tokens")) > 0) ||
				(name == "max_completion_tokens" && len(request.Field("max_tokens")) > 0)) {
			return nil
		}
	}
	return defaults[name]
}

// textOf reads a JSON string, reporting whether the value was one.
func textOf(raw json.RawMessage) (string, bool) {
	// What is not there is answered before the variable that would hold it is
	// declared: one that is decoded into is allocated whether or not it is.
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// jsonArray and jsonObject decode a value of the shape the estimate expects,
// and nothing at all for any other shape.
func jsonArray(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

// integerValue distinguishes null from an explicit integer.
func integerValue(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value *int64
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return 0, false
	}
	return *value, true
}

// multiplyBounded and addBounded saturate at MaxTokens.
func multiplyBounded(a, b int64) int64 {
	if a > MaxTokens/b {
		return MaxTokens
	}
	return a * b
}

func addBounded(a, b int64) int64 {
	if a > MaxTokens-b {
		return MaxTokens
	}
	return a + b
}
