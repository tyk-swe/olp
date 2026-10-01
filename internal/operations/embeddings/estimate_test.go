package embeddings_test

import "testing"

func TestTokenInputsReserveOnePerID(t *testing.T) {
	for _, test := range []struct {
		id, body string
		want     int64
	}{
		{"openai-embeddings", `{"model":"m","input":[[1,2,3,4,5,6,7,8],[1,2,3,4]]}`, 12},
		{"openai-embeddings", `{"model":"m","input":[1,2,3]}`, 3},
		{"tei-embeddings", `{"inputs":[[1,2,3,4,5,6,7,8],[1,2,3,4]]}`, 12},
		{"tei-embeddings", `{"inputs":[1,2,3]}`, 3},
	} {
		d := codec(t, test.id)
		view, err := d.Request(request(t, d, test.body))
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Estimate(view); got < test.want {
			t.Fatalf("%s %s: estimate=%d, want >=%d", test.id, test.body, got, test.want)
		}
	}
}
