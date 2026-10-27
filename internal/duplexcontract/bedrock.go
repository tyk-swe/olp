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
	"contentStart": true, "textOutput": true, "audioOutput": true,
	"toolUse": true, "contentEnd": true, "error": true,
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

type bedrockObserver struct{ maxBytes int }

func (o *bedrockObserver) serverFrame(frame []byte) (ServerFrame, error) {
	kind, err := bedrockEventKind(frame, o.maxBytes)
	if err != nil {
		return ServerFrame{}, err
	}
	if !bedrockServerEvents[kind] {
		return ServerFrame{}, refuse("invalid_duplex_frame", "/event", "registered_event", "Nova Sonic sent an unregistered server event.")
	}
	return ServerFrame{ProviderError: kind == "error"}, nil
}

func (o *bedrockObserver) clientFrame(frame []byte) error {
	kind, err := bedrockEventKind(frame, o.maxBytes)
	if err != nil {
		return err
	}
	if !bedrockClientEvents[kind] {
		return refuse("invalid_duplex_frame", "/event", "registered_event", "The duplex client sent an unregistered Nova Sonic event.")
	}
	return nil
}

func (o *bedrockObserver) usageValue() *wire.Usage { return nil }
func (o *bedrockObserver) complete() bool          { return false }
