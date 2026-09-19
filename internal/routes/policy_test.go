package routes

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/runtime"
)

func TestSamePolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right string
		want        bool
	}{
		{
			name: "absent policies",
			left: `null`, right: `null`, want: true,
		},
		{
			name: "absent and default policies",
			left: `null`, right: `{}`, want: true,
		},
		{
			name: "explicit default constraints",
			left: `{}`, right: `{"constraints":{"deny_data_collection":false,"require_parameters":false,"require_zero_data_retention":false,"ignore":[],"max_price":null}}`, want: true,
		},
		{
			name: "explicit default preferences",
			left: `{}`, right: `{"defaults":{"deny_data_collection":false,"require_parameters":false,"require_zero_data_retention":false,"ignore":[],"max_price":null}}`, want: true,
		},
		{
			name:  "price object key order and whitespace",
			left:  `{"constraints":{"max_price":{"input_per_million":"1","output_per_million":"2"}}}`,
			right: `{"constraints":{"max_price":{ "output_per_million": "2", "input_per_million": "1" }}}`, want: true,
		},
		{
			name:  "price changes",
			left:  `{"defaults":{"max_price":{"unit_price":"1"}}}`,
			right: `{"defaults":{"max_price":{"unit_price":"2"}}}`, want: false,
		},
		{
			name: "empty allowed strategies differ from unrestricted strategies",
			left: `{}`, right: `{"allowed_strategies":[]}`, want: false,
		},
		{
			name: "empty selectors differ from absent selectors",
			left: `{}`, right: `{"constraints":{"only":[]}}`, want: false,
		},
		{
			name: "empty order differs from absent order",
			left: `{}`, right: `{"defaults":{"order":[]}}`, want: false,
		},
		{
			name:  "selector order matters",
			left:  `{"defaults":{"order":["vendor:openai","vendor:anthropic"]}}`,
			right: `{"defaults":{"order":["vendor:anthropic","vendor:openai"]}}`, want: false,
		},
		{
			name: "enabled constraints differ from default constraints",
			left: `{}`, right: `{"constraints":{"require_parameters":true}}`, want: false,
		},
		{
			name: "explicit fallback preference differs from absent preference",
			left: `{}`, right: `{"defaults":{"allow_fallbacks":false}}`, want: false,
		},
		{
			name: "strategy changes",
			left: `{"defaults":{"strategy":"weighted"}}`, right: `{"defaults":{"strategy":"price"}}`, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var left, right *runtime.Policy
			if err := json.Unmarshal([]byte(tc.left), &left); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.right), &right); err != nil {
				t.Fatal(err)
			}
			if got := samePolicy(left, right); got != tc.want {
				t.Errorf("samePolicy(%s, %s) = %t, want %t", tc.left, tc.right, got, tc.want)
			}
		})
	}
}
