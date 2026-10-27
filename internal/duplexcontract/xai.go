package duplexcontract

import (
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func inspectXAISetup(frame []byte, maxBytes int) (Setup, error) {
	doc, err := parseObject(frame, maxBytes)
	if err != nil {
		return Setup{}, err
	}
	root := doc.Root()
	kind, _ := textField(root, "type")
	if kind != "session.update" {
		return Setup{}, nil
	}
	session, ok := root.Lookup("session")
	if !ok || session.Kind() != oif.Object {
		return Setup{}, nil
	}
	sessionType, _ := textField(session, "type")
	return Setup{TranscriptionOnly: sessionType == "transcription"}, nil
}

type xaiObserver struct {
	maxBytes          int
	usage             *wire.Usage
	pending           int
	requested         int
	inputSeq          int
	coveredSeq        int
	boundaries        map[string]int
	durationBilled    bool
	durationUncovered bool
	incomplete        bool
}

func sessionDurationBilled(session oif.Value, present bool, current bool) bool {
	if !present || session.Kind() != oif.Object {
		return current
	}
	detection, ok := session.Lookup("turn_detection")
	if !ok {
		return current
	}
	if detection.Kind() == oif.Null {
		return false
	}
	if detection.Kind() != oif.Object {
		return true
	}
	kind, present := detection.Lookup("type")
	if present && kind.Kind() == oif.Null {
		return false
	}
	return true
}

func (o *xaiObserver) applySession(session oif.Value, present bool) {
	o.durationBilled = sessionDurationBilled(session, present, o.durationBilled)
	if o.durationBilled {
		o.durationUncovered = true
	}
}

func (o *xaiObserver) serverFrame(frame []byte) (ServerFrame, error) {
	doc, err := parseObject(frame, o.maxBytes)
	if err != nil {
		return ServerFrame{}, err
	}
	root := doc.Root()
	kind, _ := textField(root, "type")
	if kind != "session.created" && kind != "session.updated" && o.durationBilled {
		o.durationUncovered = true
	}
	switch kind {
	case "error":
		return ServerFrame{ProviderError: true}, nil
	case "session.created", "session.updated":
		session, present := root.Lookup("session")
		o.applySession(session, present)
		return ServerFrame{}, nil
	case "response.created":
		if o.requested > 0 {
			o.requested--
		}
		o.pending++
		if o.boundaries == nil {
			o.boundaries = map[string]int{}
		}
		o.boundaries[responseID(root)] = o.inputSeq
		return ServerFrame{}, nil
	case "response.done":
		if o.pending > 0 {
			o.pending--
		} else if o.requested > 0 {
			o.requested--
		}
		response, ok := root.Lookup("response")
		if !ok || response.Kind() != oif.Object {
			o.incomplete = true
			return ServerFrame{}, nil
		}
		usage, ok := response.Lookup("usage")
		if !ok || usage.Kind() != oif.Object {
			o.incomplete = true
			return ServerFrame{}, nil
		}
		input, inputOK := intField(usage, "input_tokens")
		output, outputOK := intField(usage, "output_tokens")
		seconds, secondsOK := decimalField(usage, "billable_audio_seconds")
		if (!inputOK || !outputOK) && !secondsOK {
			o.incomplete = true
			return ServerFrame{}, nil
		}
		if inputOK && outputOK {
			total, totalOK := intField(usage, "total_tokens")
			o.addUsage(input, output, total, totalOK)
		}
		if secondsOK {
			o.setBillableSeconds(seconds)
		}
		o.coverInput(root)
		if !o.durationBilled && secondsOK {
			o.durationUncovered = false
		}
		return ServerFrame{}, nil
	}
	return ServerFrame{}, nil
}

func (o *xaiObserver) addUsage(input, output, total int64, hasTotal bool) {
	if o.usage == nil {
		o.usage = &wire.Usage{}
	}
	o.usage.InputTokens = addTokens(o.usage.InputTokens, input)
	o.usage.OutputTokens = addTokens(o.usage.OutputTokens, output)
	if hasTotal {
		o.usage.TotalTokens = addTokens(o.usage.TotalTokens, total)
	} else {
		o.usage.TotalTokens = addTokens(o.usage.TotalTokens, addTokens(input, output))
	}
}

func responseID(root oif.Value) string {
	response, ok := root.Lookup("response")
	if !ok || response.Kind() != oif.Object {
		return ""
	}
	id, _ := textField(response, "id")
	return id
}

func (o *xaiObserver) coverInput(root oif.Value) {
	id := responseID(root)
	boundary, known := o.boundaries[id]
	if !known {
		o.coveredSeq = o.inputSeq
		return
	}
	delete(o.boundaries, id)
	if boundary > o.coveredSeq {
		o.coveredSeq = boundary
	}
}

func (o *xaiObserver) clientFrame(frame []byte) error {
	doc, err := parseObject(frame, o.maxBytes)
	if err != nil {
		return err
	}
	root := doc.Root()
	kind, _ := textField(root, "type")
	if kind != "session.update" && o.durationBilled {
		o.durationUncovered = true
	}
	switch kind {
	case "session.update":
		session, present := root.Lookup("session")
		o.applySession(session, present)
	case "response.create":
		o.requested++
	case "input_audio_buffer.append", "input_audio_buffer.commit":
		o.inputSeq++
	case "conversation.item.create":
		o.inputSeq++
		if billableTextItem(root) {
			o.incomplete = true
		}
	}
	return nil
}

func billableTextItem(root oif.Value) bool {
	item, ok := root.Lookup("item")
	if !ok || item.Kind() != oif.Object {
		return true
	}
	if kind, _ := textField(item, "type"); kind == "function_call_output" {
		return false
	}
	content, ok := item.Lookup("content")
	if !ok || content.Kind() != oif.Array {
		return true
	}
	parts := content.Elements()
	if len(parts) == 0 {
		return true
	}
	for _, part := range parts {
		if part.Kind() != oif.Object {
			return true
		}
		switch kind, _ := textField(part, "type"); kind {
		case "input_audio", "audio":
		default:
			return true
		}
	}
	return false
}

func (o *xaiObserver) usageValue() *wire.Usage { return o.usage }
func (o *xaiObserver) complete() bool {
	return o.usage != nil && o.pending == 0 && o.requested == 0 && o.inputSeq == o.coveredSeq && !o.durationBilled && !o.durationUncovered && !o.incomplete
}

func (o *xaiObserver) setBillableSeconds(seconds string) {
	if o.usage == nil {
		o.usage = &wire.Usage{}
	}
	o.usage.MediaUnits = &seconds
}

func decimalField(parent oif.Value, name string) (string, bool) {
	v, ok := parent.Lookup(name)
	if !ok || v.Kind() != oif.Number {
		return "", false
	}
	raw := v.Raw()
	integer, fraction, hasFraction := strings.Cut(raw, ".")
	if integer == "" || len(integer) > 18 || hasFraction && (fraction == "" || len(fraction) > 6) {
		return "", false
	}
	for _, part := range [2]string{integer, fraction} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", false
			}
		}
	}
	return raw, true
}

func intField(parent oif.Value, name string) (int64, bool) {
	v, ok := parent.Lookup(name)
	if !ok || v.Kind() != oif.Number {
		return 0, false
	}
	n, err := strconv.ParseInt(v.Raw(), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func addTokens(a, b int64) int64 {
	const limit = int64(1) << 61
	if a > limit-b {
		return limit
	}
	return a + b
}
