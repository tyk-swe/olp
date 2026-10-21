package oif

import (
	"strings"
	"testing"
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
