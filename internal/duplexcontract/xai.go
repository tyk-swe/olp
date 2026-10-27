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
	maxBytes   int
	usage      *wire.Usage
	pending    int
	incomplete bool
}

func (o *xaiObserver) serverFrame(frame []byte) (ServerFrame, error) {
	doc, err := parseObject(frame, o.maxBytes)
	if err != nil {
		return ServerFrame{}, err
	}
	root := doc.Root()
	kind, _ := textField(root, "type")
	switch kind {
	case "error":
		return ServerFrame{ProviderError: true}, nil
	case "response.created":
		o.pending++
		return ServerFrame{}, nil
	case "response.done":
		if o.pending > 0 {
			o.pending--
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

func (o *xaiObserver) clientFrame(frame []byte) error {
	_, err := parseObject(frame, o.maxBytes)
	return err
}

func (o *xaiObserver) usageValue() *wire.Usage { return o.usage }
func (o *xaiObserver) complete() bool {
	return o.usage != nil && o.pending == 0 && !o.incomplete
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
