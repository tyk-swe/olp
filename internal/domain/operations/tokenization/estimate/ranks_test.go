package estimate

import (
	"testing"
)

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// Three tokens, of lengths 1, 1 and 2.

// byteTokens is every single-byte token, which a rank file must start with
// for newVocab to accept it.
func byteTokens() (lengths, blob []byte) {
	for b := range 256 {
		lengths = append(lengths, 1)
		blob = append(blob, byte(b))
	}
	return lengths, blob
}

func TestNewVocabRejectsBadRankFiles(t *testing.T) {
	lengths, blob := byteTokens()
	valid := append(append(appendUvarint([]byte(rankMagic), 256), lengths...), blob...)
	if _, err := newVocab(string(valid)); err != nil {
		t.Fatalf("the smallest valid rank file was rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"wrong magic", "OLPRANK2" + string(valid[len(rankMagic):])},
		{"no count", rankMagic},
		{"zero tokens", rankMagic + "\x00"},
		{"count beyond the packed rank", string(appendUvarint([]byte(rankMagic), rankMask+1))},
		{"lengths beyond four gigabytes", string(appendUvarint(appendUvarint(appendUvarint([]byte(rankMagic), 2), 1<<32), 1))},
		{"a repeated byte token", string(append(append(appendUvarint([]byte(rankMagic), 257), append(lengths, 1)...), append(append([]byte{}, blob...), 'A')...))},
		{"a repeated pair", string(appendUvarint([]byte(rankMagic), 258)) + string(lengths) + "\x02\x02" + string(blob) + "abab"},
		{"a repeated long token", string(appendUvarint([]byte(rankMagic), 258)) + string(lengths) + "\x03\x03" + string(blob) + "abcabc"},
		{"truncated lengths", string(valid[:len(rankMagic)+3])},
		{"truncated bytes", string(valid[:len(valid)-1])},
		{"trailing bytes", string(valid) + "x"},
		{"zero-length token", string(append(append(appendUvarint([]byte(rankMagic), 256), append([]byte{0}, lengths[1:]...)...), blob...))},
		{"missing a byte token", string(append(append(appendUvarint([]byte(rankMagic), 256), lengths...), append(append([]byte{}, blob[:255]...), 'A')...))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newVocab(tc.data); err == nil {
				t.Fatal("newVocab accepted a corrupt rank file")
			}
		})
	}
}

// A token with one more byte, or one fewer, is only a token if
// the list says so.
