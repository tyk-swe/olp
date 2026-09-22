package media

import (
	"bytes"
	"testing"
)

func TestReservationConfigurationRetainsNativeIdentity(t *testing.T) {
	for _, test := range []struct {
		name, before, after string
		equal               bool
	}{
		{"negative zero", `{"options":{"parameter_defaults":{"seed":-0}}}`, `{"options":{"parameter_defaults":{"seed":0}}}`, false},
		{"long decimal", `{"options":{"parameter_defaults":{"temperature":0.1000000000000000000001}}}`, `{"options":{"parameter_defaults":{"temperature":0.1}}}`, false},
		{"unsafe integer", `{"options":{"parameter_defaults":{"seed":9007199254740993}}}`, `{"options":{"parameter_defaults":{"seed":9007199254740992}}}`, false},
		{"null presence", `{"options":{"parameter_defaults":{"temperature":null}}}`, `{"options":{"parameter_defaults":{}}}`, false},
		{"array order", `{"options":{"parameter_defaults":{"stop":["a","b"]}}}`, `{"options":{"parameter_defaults":{"stop":["b","a"]}}}`, false},
		{"profile", `{"profile_id":"compatible-chat","profile_revision":"1"}`, `{"profile_id":"compatible-responses","profile_revision":"1"}`, false},
		{"quota-only edit", `{"options":{"limits":{"max_concurrency":1},"parameter_defaults":{"seed":-0}}}`, `{"options":{"limits":{"max_concurrency":2},"parameter_defaults":{"seed":-0}}}`, true},
		{"whitespace", `{"options":{"parameter_defaults":{"seed": -0}}}`, `{"options":{"parameter_defaults":{"seed":-0}}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, err := reservationConfiguration([]byte(test.before))
			if err != nil {
				t.Fatal(err)
			}
			after, err := reservationConfiguration([]byte(test.after))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, after) != test.equal {
				t.Fatal("media reservation changed the native configuration comparison contract")
			}
		})
	}
}
