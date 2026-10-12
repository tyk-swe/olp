package protocols

import (
	"encoding/base64"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/oif"
)

// BedrockObservation describes native completion evidence without rewriting
// the EventStream message. Usage is projected from this one bounded source; delivery remains caller-owned.
type BedrockObservation struct {
	Source   oif.Document
	Native   oif.Document
	Kind     string
	Terminal bool
	Failed   bool
	Usage    BedrockUsageObservation
}

func ObserveBedrockEvent(message *eventstream.Message, limit int) (BedrockObservation, error) {
	header := func(name string) string {
		if value := message.Headers.Get(name); value != nil {
			return value.String()
		}
		return ""
	}
	observation := BedrockObservation{Kind: header(":event-type")}
	source, err := oif.ParseJSON(message.Payload, oif.Limits{MaxBytes: limit})
	if err != nil || source.Root().Kind() != oif.Object {
		return observation, protocolError("invalid native Bedrock event")
	}
	observation.Source = source
	if header(":message-type") != "event" {
		observation.Failed = true
		return observation, nil
	}
	switch observation.Kind {
	case "messageStop":
		reason, ok := source.Root().Lookup("stopReason")
		text, valid := reason.Chars()
		if !ok || !valid || text == "" {
			return observation, protocolError("invalid Converse terminal event")
		}
		observation.Terminal = true
	case "chunk":
		encoded, ok := source.Root().Lookup("bytes")
		if !ok || encoded.Kind() != oif.String {
			return observation, protocolError("invalid native Bedrock chunk")
		}
		text, _ := encoded.Chars()
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return observation, protocolError("invalid native Bedrock chunk encoding")
		}
		native, err := oif.ParseJSON(decoded, oif.Limits{MaxBytes: limit})
		if err != nil || native.Root().Kind() != oif.Object {
			return observation, protocolError("invalid native Bedrock chunk document")
		}
		observation.Native = native
		kind, _ := native.Root().Lookup("type")
		observation.Kind, _ = kind.Chars()
		observation.Terminal = observation.Kind == "message_stop"
		observation.Failed = observation.Kind == "error"
		if !observation.Failed {
			metrics, _ := native.Root().Lookup("amazon-bedrock-invocationMetrics")
			observation.Usage, err = observedBedrockUsage(metrics, invocationUsageNames, false)
			if err != nil {
				return observation, err
			}
			if observation.Usage.Value != nil {
				observation.Usage.Authoritative = observation.Usage.InputPresent && observation.Usage.OutputPresent
			} else {
				var usage oif.Value
				switch observation.Kind {
				case "message_start":
					message, _ := native.Root().Lookup("message")
					usage, _ = message.Lookup("usage")
				case "message_delta":
					usage, _ = native.Root().Lookup("usage")
				}
				observation.Usage, err = observedBedrockUsage(usage, anthropicUsageNames, true)
				if err != nil {
					return observation, err
				}
			}
		}
	case "metadata":
		usage, _ := source.Root().Lookup("usage")
		observation.Usage, err = observedBedrockUsage(usage, converseUsageNames, false)
		if err != nil {
			return observation, err
		}
		observation.Usage.Authoritative = observation.Usage.InputPresent && observation.Usage.OutputPresent
	}
	return observation, nil
}
