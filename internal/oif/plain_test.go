package oif

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

// plainEndByByte is what plainEnd is for, read one byte at a time.
func plainEndByByte(s string, i int) int {
	for ; i < len(s); i++ {
		if c := s[i]; c == '"' || c == '\\' || c < 0x20 {
			break
		}
	}
	return i
}

// Every byte value in every place of a word, among bytes that are plain, and
// among those that sit next to the ones that fool the arithmetic.

// Random bytes, mostly plain, with a few of any kind.

// A long run with one byte past its end.

func BenchmarkPlainEnd(b *testing.B) {
	text := strings.Repeat("The quarterly report covers revenue, churn and support load across every region. ", 5000)
	b.SetBytes(int64(len(text)))
	b.Run("words", func(b *testing.B) {
		for range b.N {
			plainEnd(text, 0)
		}
	})
	b.Run("bytes", func(b *testing.B) {
		for range b.N {
			plainEndByByte(text, 0)
		}
	})
}

func TestStringsOfTheCorporaDecodeAsEncodingJSONDecodesThem(t *testing.T) {
	var texts int
	for name, data := range testutil.Requests(t) {
		doc, err := ParseJSON(data, Limits{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		texts += checkTexts(t, name, doc.Root())
	}
	for name, data := range testutil.Documents(t) {
		doc, err := ParseJSON(data, Limits{MaxDepth: 1 << 10})
		if err != nil {
			continue // not a document the parser takes: duplicate names, say
		}
		texts += checkTexts(t, name, doc.Root())
	}
	if texts < 1000 {
		t.Errorf("%d strings in the corpora: they are not exercising the decoder", texts)
	}
}

func checkTexts(t *testing.T, name string, v Value) (count int) {
	switch v.Kind() {
	case String:
		var want string
		if err := json.Unmarshal([]byte(v.Raw()), &want); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, ok := v.Text(); !ok || got != want {
			t.Fatalf("%s: Text of %.80q is %.80q, want %.80q", name, v.Raw(), got, want)
		}
		if got, ok := v.Chars(); !ok || got != want {
			t.Fatalf("%s: Chars of %.80q is %.80q, want %.80q", name, v.Raw(), got, want)
		}
		return 1
	case Array:
		for _, e := range v.Elements() {
			count += checkTexts(t, name, e)
		}
	case Object:
		for _, m := range v.Members() {
			count += checkTexts(t, name, m.Value)
		}
	}
	return count
}
