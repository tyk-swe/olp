package openai

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestExplicitTokenLimitSuppressesDefaultsUnderEitherAlias(t *testing.T) {
	for _, explicit := range []string{"max_tokens", "max_completion_tokens"} {
		for _, value := range []string{"32", "null"} {
			t.Run(explicit+"/"+value, func(t *testing.T) {
				r, err := Parse(FamilyChat, []byte(`{"model":"r","messages":[{"role":"user","content":"hi"}],"`+explicit+`":`+value+`}`))
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := r.Encode("upstream", map[string]json.RawMessage{
					"max_tokens": json.RawMessage("64"), "max_completion_tokens": json.RawMessage("128"), "temperature": json.RawMessage("0.5"),
				})
				if err != nil {
					t.Fatal(err)
				}
				// The composed document must still satisfy the mutual exclusion rule.
				out, err := Parse(FamilyChat, encoded)
				if err != nil {
					t.Fatalf("defaults invalidated the request: %s: %v", encoded, err)
				}
				if string(out.Field(explicit)) != value || string(out.Field("temperature")) != "0.5" {
					t.Fatalf("explicit value or unrelated default changed: %s", encoded)
				}
			})
		}
	}
	for _, name := range []string{"max_tokens", "max_completion_tokens"} {
		r, err := Parse(FamilyChat, []byte(`{"model":"r","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := r.Encode("upstream", map[string]json.RawMessage{name: json.RawMessage("64")})
		if err != nil {
			t.Fatal(err)
		}
		out, err := Parse(FamilyChat, encoded)
		if err != nil || string(out.Field(name)) != "64" {
			t.Fatalf("absent token limit did not use default: %s: %v", encoded, err)
		}
	}
}

func TestTopLogprobsAcceptsZeroAndPreservesValue(t *testing.T) {
	for _, family := range []Family{FamilyChat, FamilyResponses} {
		for _, value := range []string{"0", "1", "20", "null", "-1", "21", "0.5", `"0"`, "true", "{}", "[]"} {
			t.Run(string(family)+"/"+value, func(t *testing.T) {
				input := `"messages":[{"role":"user","content":"hi"}],"logprobs":true`
				if family == FamilyResponses {
					input = `"input":"hi"`
				}
				body := []byte(`{"model":"r",` + input + `,"top_logprobs":` + value + `}`)
				request, err := Parse(family, body)
				switch value {
				case "0", "1", "20", "null":
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := request.Encode("upstream", nil)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(encoded, &fields); err != nil {
						t.Fatal(err)
					}
					if string(fields["top_logprobs"]) != value {
						t.Fatalf("top_logprobs changed in upstream request: %s", encoded)
					}
				default:
					var invalid *RequestError
					if !errors.As(err, &invalid) || invalid.Code != "invalid_value" || invalid.Param != "top_logprobs" {
						t.Fatalf("invalid top_logprobs: %v", err)
					}
				}
			})
		}
	}
}

func TestChatStreamUsageOptionMustBeBoolean(t *testing.T) {
	for _, value := range []string{`"false"`, "0", "1", "[]", "{}"} {
		t.Run(value, func(t *testing.T) {
			_, err := Parse(FamilyChat, []byte(`{"model":"r","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":`+value+`}}`))
			var invalid *RequestError
			if !errors.As(err, &invalid) || invalid.Param != "stream_options.include_usage" {
				t.Fatalf("invalid usage option was accepted: %v", err)
			}
		})
	}
}
