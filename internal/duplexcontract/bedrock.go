package duplexcontract

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var bedrockClientEvents = map[string]bool{
	"sessionStart": true, "promptStart": true, "contentStart": true,
	"textInput": true, "audioInput": true, "imageInput": true, "toolResult": true,
	"contentEnd": true, "promptEnd": true, "sessionEnd": true,
}

var bedrockServerEvents = map[string]bool{
	"completionStart": true, "contentStart": true, "textOutput": true, "audioOutput": true,
	"toolUse": true, "contentEnd": true, "usageEvent": true, "completionEnd": true, "error": true,
}

func bedrockEventKind(frame []byte, maxBytes int) (string, error) {
	doc, err := parseObject(frame, maxBytes)
	if err != nil {
		return "", err
	}
	event, ok := doc.Root().Lookup("event")
	if !ok || event.Kind() != oif.Object || len(event.Members()) != 1 {
		return "", refuse("invalid_duplex_frame", "/event", "one_event_object", "Nova Sonic frames must carry one event object.")
	}
	return event.Members()[0].Name, nil
}

func inspectBedrockSetup(frame []byte, maxBytes int) (Setup, error) {
	kind, err := bedrockEventKind(frame, maxBytes)
	if err != nil {
		return Setup{}, err
	}
	if kind != "sessionStart" {
		return Setup{}, refuse("invalid_duplex_setup", "/event", "session_start_first", "A Nova Sonic session must begin with sessionStart.")
	}
	return Setup{}, nil
}

type bedrockObserver struct {
	maxBytes        int
	usage           *wire.Usage
	pending         int
	inputAfterUsage bool
	sawUsage        bool
	incomplete      bool
}

func (o *bedrockObserver) serverFrame(frame []byte) (ServerFrame, error) {
	doc, err := parseObject(frame, o.maxBytes)
	if err != nil {
		return ServerFrame{}, err
	}
	event, ok := doc.Root().Lookup("event")
	if !ok || event.Kind() != oif.Object || len(event.Members()) != 1 {
		return ServerFrame{}, refuse("invalid_duplex_frame", "/event", "one_event_object", "Nova Sonic frames must carry one event object.")
	}
	kind := event.Members()[0].Name
	if !bedrockServerEvents[kind] {
		return ServerFrame{}, refuse("invalid_duplex_frame", "/event", "registered_event", "Nova Sonic sent an unregistered server event.")
	}
	switch kind {
	case "completionStart":
		o.pending++
		o.sawUsage = false
	case "completionEnd":
		if o.pending > 0 {
			o.pending--
		}
		if !o.sawUsage {
			o.incomplete = true
		}
	case "usageEvent":
		if usage, valid := bedrockUsage(event.Members()[0].Value); valid {
			if o.usage == nil || usage.TotalTokens >= o.usage.TotalTokens {
				o.usage = usage
			}
			o.inputAfterUsage = false
			o.sawUsage = true
		} else {
			o.incomplete = true
		}
	}
	return ServerFrame{ProviderError: kind == "error"}, nil
}

func bedrockUsage(event oif.Value) (*wire.Usage, bool) {
	if event.Kind() != oif.Object {
		return nil, false
	}
	input, inputOK := intField(event, "totalInputTokens")
	output, outputOK := intField(event, "totalOutputTokens")
	total, totalOK := intField(event, "totalTokens")
	if !inputOK || !outputOK || !totalOK {
		return nil, false
	}
	usage := &wire.Usage{InputTokens: input, OutputTokens: output, TotalTokens: total}
	details, ok := event.Lookup("details")
	if !ok || details.Kind() != oif.Object {
		return usage, true
	}
	totals, ok := details.Lookup("total")
	if !ok || totals.Kind() != oif.Object {
		return usage, true
	}
	if in, ok := totals.Lookup("input"); ok && in.Kind() == oif.Object {
		if speech, speechOK := intField(in, "speechTokens"); speechOK {
			usage.AudioInputTokens = &speech
		}
	}
	if out, ok := totals.Lookup("output"); ok && out.Kind() == oif.Object {
		if speech, speechOK := intField(out, "speechTokens"); speechOK {
			usage.AudioOutputTokens = &speech
		}
	}
	return usage, true
}

func (o *bedrockObserver) clientFrame(frame []byte) error {
	kind, err := bedrockEventKind(frame, o.maxBytes)
	if err != nil {
		return err
	}
	if !bedrockClientEvents[kind] {
		return refuse("invalid_duplex_frame", "/event", "registered_event", "The duplex client sent an unregistered Nova Sonic event.")
	}
	if kind == "textInput" || kind == "audioInput" || kind == "imageInput" || kind == "toolResult" {
		o.inputAfterUsage = true
	}
	return nil
}

func (o *bedrockObserver) usageValue() *wire.Usage { return o.usage }
func (o *bedrockObserver) pendingResponse() bool   { return o.pending > 0 }
func (o *bedrockObserver) complete() bool {
	return o.usage != nil && o.pending == 0 && !o.inputAfterUsage && !o.incomplete
}
