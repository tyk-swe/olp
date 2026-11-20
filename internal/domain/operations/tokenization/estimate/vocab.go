package estimate

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// A rank file holds one encoding's mergeable tokens in rank order. It is the
// token list of the public .tiktoken file in a compact form: the magic, the
// token count and each token's length as uvarints, then the token bytes back
// to back. The rank of a token is its index, which the public file also
// guarantees. ranks_test.go rebuilds the public text from this form and
// checks it against the SHA-256 OpenAI publishes.
const rankMagic = "OLPRANK1"

// The lookup table packs a rank and part of the token's hash into 32 bits.
const (
	rankBits = 18
	rankMask = 1<<rankBits - 1
	tagBits  = 32 - rankBits
)

// noRank marks a pair of parts that does not merge into a token.
const noRank = math.MaxUint32

// vocab maps token bytes to ranks and back. Lookups hash into an open
// addressing table of ranks, so a probe costs one or two cache lines and a
// whole encoding takes a few megabytes, where a Go map of strings would take
// several times that and spend startup on allocating its keys.
type vocab struct {
	// blob holds every token back to back, in rank order, and offsets[r] is
	// where token r starts. The blob aliases the embedded data.
	blob    string
	offsets []uint32
	// table holds the tokens of three bytes or more: rank+1 in its low bits and
	// the hash tag above them, with zero for an empty slot. The tag rejects most
	// colliding probes without touching the blob.
	table []uint32
	mask  uint32
	// longest is the length of the longest token, so a long piece is known not
	// to be one without hashing it.
	longest int
	// bytes[b] is the rank of the single byte b. Every byte has a token.
	bytes [256]uint32
	// pairs[a<<8|b] is rank+1 of the two-byte token ab, or zero. Most merges
	// start from pairs, and this skips the hash for them.
	pairs []uint32
}

// newVocab builds the lookup structures for a rank file.
func newVocab(data string) (*vocab, error) {
	if !strings.HasPrefix(data, rankMagic) {
		return nil, errors.New("not a rank file")
	}
	rest := data[len(rankMagic):]
	count, n := uvarint(rest)
	if n <= 0 || count == 0 || count >= rankMask {
		return nil, errors.New("rank file has a bad token count")
	}
	rest = rest[n:]
	v := &vocab{offsets: make([]uint32, count+1), pairs: make([]uint32, 1<<16)}
	var total uint64
	for i := range count {
		length, n := uvarint(rest)
		if n <= 0 || length == 0 {
			return nil, fmt.Errorf("rank file has a bad length for token %d", i)
		}
		rest = rest[n:]
		total += length
		if total > math.MaxUint32 {
			return nil, errors.New("rank file is too large")
		}
		v.offsets[i+1] = uint32(total)
	}
	if uint64(len(rest)) != total {
		return nil, fmt.Errorf("rank file holds %d token bytes, want %d", len(rest), total)
	}
	v.blob = rest
	size := uint32(1) << 8
	for uint64(size) < count*5/2 {
		size <<= 1
	}
	v.table, v.mask = make([]uint32, size), size-1
	for r := range v.bytes {
		v.bytes[r] = noRank
	}
	for r := range uint32(count) {
		token := v.token(r)
		v.longest = max(v.longest, len(token))
		switch len(token) {
		case 1:
			if v.bytes[token[0]] != noRank {
				return nil, fmt.Errorf("rank file repeats token %#x", token)
			}
			v.bytes[token[0]] = r
		case 2:
			slot := &v.pairs[int(token[0])<<8|int(token[1])]
			if *slot != 0 {
				return nil, fmt.Errorf("rank file repeats token %q", token)
			}
			*slot = r + 1
		default:
			if !v.insert(token, r) {
				return nil, fmt.Errorf("rank file repeats token %q", token)
			}
		}
	}
	for b, r := range v.bytes {
		if r == noRank {
			return nil, fmt.Errorf("rank file has no token for byte %#x", b)
		}
	}
	return v, nil
}

// uvarint decodes a varint from the start of s, returning its width, or 0 when
// s does not start with a complete one.
func uvarint(s string) (value uint64, width int) {
	for i := 0; i < len(s) && i < 10; i++ {
		b := s[i]
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1
		}
	}
	return 0, 0
}

func (v *vocab) token(rank uint32) string {
	return v.blob[v.offsets[rank]:v.offsets[rank+1]]
}

// hash splits a token's hash into the slot it starts probing at and the tag
// stored beside its rank, which use different bits.
func (v *vocab) hash(token string) (slot, tag uint32) {
	h := hashToken(token)
	return uint32(h) & v.mask, uint32(h>>40) & (1<<tagBits - 1)
}

// The hash multiplies two words of the token into one and folds the halves of
// the product, which is the core of wyhash. Tokens are short, so most of them
// take one multiplication. It has no seed: the keys are the fixed vocabulary, so
// a lookup cannot probe further than the longest cluster the vocabulary makes.
const (
	hashMul1 = 0xa0761d6478bd642f
	hashMul2 = 0xe7037ed1a0b428db
)

func hashToken(s string) uint64 {
	n := len(s)
	h := uint64(n)
	for len(s) > 16 {
		hi, lo := bits.Mul64(load64(s, 0)^hashMul1, load64(s, 8)^h)
		h = hi ^ lo
		s = s[16:]
	}
	var a, b uint64
	switch {
	case len(s) >= 8:
		a, b = load64(s, 0), load64(s, len(s)-8)
	case len(s) >= 4:
		a, b = uint64(load32(s, 0)), uint64(load32(s, len(s)-4))
	case len(s) > 0:
		a, b = uint64(s[0])<<16|uint64(s[len(s)>>1])<<8|uint64(s[len(s)-1]), 0
	}
	hi, lo := bits.Mul64(a^hashMul2, b^h^hashMul1)
	return hi ^ lo
}

func load32(s string, i int) uint32 {
	return uint32(s[i]) | uint32(s[i+1])<<8 | uint32(s[i+2])<<16 | uint32(s[i+3])<<24
}

func load64(s string, i int) uint64 {
	return uint64(load32(s, i)) | uint64(load32(s, i+4))<<32
}

// insert adds a token of three bytes or more, and reports false if the table
// already holds it.
func (v *vocab) insert(token string, rank uint32) bool {
	slot, tag := v.hash(token)
	for v.table[slot] != 0 {
		if e := v.table[slot]; e>>rankBits == tag && v.token(e&rankMask-1) == token {
			return false
		}
		slot = (slot + 1) & v.mask
	}
	v.table[slot] = tag<<rankBits | (rank + 1)
	return true
}

// rank returns the rank of a token, or noRank when the bytes are not one.
func (v *vocab) rank(token string) uint32 {
	switch len(token) {
	case 0:
		return noRank
	case 1:
		return v.bytes[token[0]]
	case 2:
		if r := v.pairs[int(token[0])<<8|int(token[1])]; r != 0 {
			return r - 1
		}
		return noRank
	}
	if len(token) > v.longest {
		return noRank
	}
	slot, tag := v.hash(token)
	for {
		e := v.table[slot]
		if e == 0 {
			return noRank
		}
		if e>>rankBits == tag {
			if r := e&rankMask - 1; v.token(r) == token {
				return r
			}
		}
		slot = (slot + 1) & v.mask
	}
}
