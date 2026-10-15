package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
)

func (x *call) redactBedrockFailure(message *eventstream.Message, observation protocols.BedrockObservation) error {
	source := observation.Source
	if observation.Native.Valid() {
		redacted, err := redactNativeFailureDocument(observation.Native, x.sensitive)
		if err != nil {
			return err
		}
		if redacted != observation.Native {
			encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(redacted.Bytes()))
			source, err = oif.Apply(source, []oif.Change{{Pointer: "/bytes", Value: string(encoded), Origin: oif.ExplicitTransform, Reason: "provider error credential redaction"}})
			if err != nil {
				return err
			}
		}
	} else {
		var err error
		source, err = redactNativeFailureDocument(source, x.sensitive)
		if err != nil {
			return err
		}
	}
	message.Payload = source.Bytes()
	for index, header := range message.Headers {
		if x.sensitive.Contains(header.Name) {
			return errors.New("native error has a credential-bearing header name")
		}
		if value, ok := header.Value.(eventstream.StringValue); ok {
			message.Headers[index].Value = eventstream.StringValue(x.sensitive.Redact(string(value)))
		}
	}
	return nil
}
