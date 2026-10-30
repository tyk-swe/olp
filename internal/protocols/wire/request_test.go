package wire

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestTransformationsReturnErrorsWithoutChangingSource(t *testing.T) {
	source, err := FromFields(Chat, "route", false, map[string]json.RawMessage{"model": json.RawMessage(`"route"`), "messages": json.RawMessage(`[{"role":"user","content":"hello"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	before := source.OIF().Document()
	for _, transform := range []func() (*Request, error){
		func() (*Request, error) { return source.WithField("messages", json.RawMessage(`[`)) },
		func() (*Request, error) {
			return source.WithFields(map[string]json.RawMessage{"model": json.RawMessage(`"route"`), "input": json.RawMessage(`{"key":1,"key":2}`)}, oif.ExplicitTransform)
		},
	} {
		next, err := transform()
		if err == nil || next != nil {
			t.Fatalf("invalid transformation produced %v, %v", next, err)
		}
		if source.OIF().Document() != before {
			t.Fatal("failed transformation changed immutable source")
		}
	}
	next, err := source.WithField("user", json.RawMessage(`"reader"`))
	if err != nil {
		t.Fatal(err)
	}
	if source.Field("user") != nil || !bytes.Equal(next.Field("user"), []byte(`"reader"`)) {
		t.Fatal("successful transformation changed its input")
	}
	if _, err := FromFields(Chat, "route", false, map[string]json.RawMessage{"input": json.RawMessage(`[`)}); err == nil {
		t.Fatal("invalid fields were deferred")
	}
	if _, err := NewRequest(Chat, "route", false, oif.Document{}); err == nil {
		t.Fatal("invalid source was deferred")
	}
}

func TestValidatedStreamingRequestsRetainNativeBytesAndDelivery(t *testing.T) {
	for _, test := range []struct {
		format Format
		body   string
	}{
		{Chat, "{\n  \"mo\\u0064el\" : \"team-code\", \"stream\":true, \"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\n  \"\\u0065xtension\" : { \"number\":1.2300, \"model\":\"team-code\" }\n}"},
		{Responses, "{\n  \"mo\\u0064el\" : \"team-code\", \"stream\":true, \"input\":\"hello\",\n  \"\\u0065xtension\" : { \"number\":1.2300, \"model\":\"team-code\" }\n}"},
		{Anthropic, "{\n  \"mo\\u0064el\" : \"team-code\", \"stream\":true, \"max_tokens\":8, \"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\n  \"\\u0065xtension\" : { \"number\":1.2300, \"model\":\"team-code\" }\n}"},
	} {
		t.Run(string(test.format), func(t *testing.T) {
			document, err := oif.ParseJSON([]byte(test.body), oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			request, err := NewRequest(test.format, "team-code", false, document)
			if err != nil {
				t.Fatal(err)
			}
			request.Stream = true
			validated := request.WithValidation("fixture-codec")
			if !validated.ValidatedBy("fixture-codec") || validated.ValidatedBy("another-codec") || request.ValidatedBy("fixture-codec") {
				t.Fatal("validation did not remain specific to the returned request and codec")
			}
			prepared, err := oif.PrepareIdentity(validated.OIF(), Descriptor(validated.Family, validated.Stream), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(prepared.Document().Bytes(), []byte(test.body)) || !bytes.Equal(request.OIF().Source().Bytes(), []byte(test.body)) {
				t.Fatalf("native validation changed caller bytes %s", prepared.Document().Bytes())
			}
			if prepared.Request().Descriptor().Execution.Delivery != prepared.Descriptor().Execution.Delivery {
				t.Fatalf("validated native stream retained unary delivery %+v", prepared.Request().Descriptor().Execution)
			}
			if request.OIF().Descriptor().Execution.Delivery != Descriptor(test.format, false).Execution.Delivery {
				t.Fatal("validation changed the input request's delivery")
			}
		})
	}
}

func TestModelTransformsInvalidateValidationWithoutBlessingNativeIdentity(t *testing.T) {
	body := []byte("{\n \"model\" : \"team-code\", \"input\":\"hello\", \"extension\": { \"model\" : \"team-code\", \"number\":1.2300 }\n}")
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewRequest(Responses, "team-code", false, document)
	if err != nil {
		t.Fatal(err)
	}
	validated := request.WithValidation("fixture-codec")
	rewritten, err := validated.WithFields(map[string]json.RawMessage{
		"model":     json.RawMessage(`"native-model"`),
		"input":     validated.Field("input"),
		"extension": validated.Field("extension"),
	}, oif.ExplicitTransform)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten.ValidatedBy("fixture-codec") || !validated.ValidatedBy("fixture-codec") {
		t.Fatal("model transformation reused or damaged validation of the original document")
	}
	if !bytes.Equal(validated.Field("model"), []byte(`"team-code"`)) || !bytes.Equal(rewritten.Document()["model"], []byte(`"native-model"`)) || !bytes.Equal(rewritten.Field("extension"), validated.Field("extension")) {
		t.Fatalf("model transformation changed unrelated payload %s", rewritten.OIF().Document().Bytes())
	}
	if !bytes.Equal(rewritten.OIF().Source().Bytes(), body) || !bytes.Equal(validated.OIF().Document().Bytes(), body) {
		t.Fatal("transformation discarded or changed the native source")
	}
	revalidated := rewritten.WithValidation("fixture-codec")
	if !revalidated.ValidatedBy("fixture-codec") {
		t.Fatal("derived document could not receive its own validation")
	}
	if _, err := oif.PrepareIdentity(revalidated.OIF(), Descriptor(revalidated.Family, revalidated.Stream), nil); err == nil {
		t.Fatal("codec validation allowed model rewriting to claim native identity")
	}
}
