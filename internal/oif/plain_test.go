package oif

import (
	"encoding/json"
	"math/rand/v2"
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

// plainEnd reads eight bytes at a time and must still stop at the byte a string
// cannot hold, wherever in a word it is, whatever surrounds it, and whatever the
// bytes before it are: the arithmetic that finds it is exact only if it is
// written right, and a byte that it missed would be a string that was accepted
// with a control character in it.
func TestPlainEndStopsAtTheFirstByteAStringCannotHold(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	special := func(c byte) bool { return c == '"' || c == '\\' || c < 0x20 }
	check := func(s string, i int) {
		t.Helper()
		if got, want := plainEnd(s, i), plainEndByByte(s, i); got != want {
			t.Fatalf("plainEnd(%q, %d) = %d, want %d", s, i, got, want)
		}
	}
	// Every byte value in every place of a word, among bytes that are plain, and
	// among those that sit next to the ones that fool the arithmetic.
	for _, filler := range []byte{'a', ' ', 0x21, 0x7f, 0x80, 0xc3, 0xff, 0x20, 0x23, 0x5b, 0x5d} {
		for value := range 256 {
			for position := range 17 {
				b := []byte(strings.Repeat(string(filler), 17))
				b[position] = byte(value)
				for start := range 9 {
					check(string(b), start)
				}
			}
		}
	}
	// Random bytes, mostly plain, with a few of any kind.
	for range 20000 {
		b := make([]byte, rng.IntN(70))
		for i := range b {
			switch rng.IntN(12) {
			case 0:
				b[i] = byte(rng.IntN(256))
			case 1:
				b[i] = "\"\\\n\t\x00\x1f \x7f\x80\xff"[rng.IntN(10)]
			default:
				b[i] = byte(0x20 + rng.IntN(0x5f))
				if special(b[i]) {
					b[i] = 'x'
				}
			}
		}
		for _, start := range []int{0, 1, 3, 7, 8, 9, len(b) / 2, len(b)} {
			if start <= len(b) {
				check(string(b), start)
			}
		}
	}
	// A long run with one byte past its end.
	for _, length := range []int{0, 1, 7, 8, 9, 15, 16, 17, 1 << 10, 400_000} {
		plain := strings.Repeat("The report says: ok. ", length/21+1)[:length]
		for _, tail := range []string{"", "\"", "\\", "\n", "\x00", "\x1f"} {
			check(plain+tail, 0)
			check(plain+tail+"after", 0)
		}
	}
}

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

// The same, on the documents of the corpora: wherever a string of them has a byte
// that a string cannot hold as it is, and from every alignment of the word before
// it.
func TestPlainEndStopsWhereTheCorporaHaveTheirSpecialBytes(t *testing.T) {
	inputs := map[string][]byte{}
	for name, data := range testutil.Documents(t) {
		inputs[name] = data
	}
	for name, data := range testutil.Requests(t) {
		inputs[name] = data
	}
	var stops int
	for name, data := range inputs {
		s := string(data)
		for i := 0; i <= len(s); {
			want := plainEndByByte(s, i)
			for offset := range 9 {
				if from := i + offset; from <= len(s) {
					if got, want := plainEnd(s, from), plainEndByByte(s, from); got != want {
						t.Fatalf("%s: plainEnd(_, %d) = %d, want %d", name, from, got, want)
					}
				}
			}
			stops++
			if want >= len(s) {
				break
			}
			i = want + 1
		}
	}
	if stops < 10000 {
		t.Errorf("the corpora have %d stops to check: they are not exercising the scan", stops)
	}
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
