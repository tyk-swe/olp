package estimate

import (
	_ "embed"
	"fmt"
	"sync"
)

//go:embed ranks/o200k_base.bin
var o200kRanks string

//go:embed ranks/cl100k_base.bin
var cl100kRanks string

// Encoding is one of OpenAI's public byte-pair encodings. Its rank file is
// parsed on first use, so a process that never estimates pays nothing for it
// beyond its place in the binary.
type Encoding struct {
	name  string
	ranks string
	scan  scanner

	once    sync.Once
	vocab   *vocab
	classes *classTable
	err     error
}

var (
	// O200kBase is the encoding of the GPT-4o, GPT-4.1, GPT-5 and o-series
	// models.
	O200kBase = &Encoding{name: "o200k_base", ranks: o200kRanks, scan: scanO200k}
	// CL100kBase is the encoding of GPT-4, GPT-3.5 Turbo and the third
	// generation of OpenAI embedding models.
	CL100kBase = &Encoding{name: "cl100k_base", ranks: cl100kRanks, scan: scanCL100k}
)

// Name is the encoding's name in tiktoken.
func (e *Encoding) Name() string { return e.name }

// load parses the rank file once. The data is compiled into the binary and
// checked by the tests, so a failure here means a corrupt binary; callers that
// can degrade fall back to the heuristic rather than fail a request.
func (e *Encoding) load() error {
	e.once.Do(func() {
		e.classes = classes()
		if e.vocab, e.err = newVocab(e.ranks); e.err != nil {
			e.err = fmt.Errorf("%s: %w", e.name, e.err)
		}
	})
	return e.err
}

// Count returns the number of tokens in text, as tiktoken's encode_ordinary
// counts them. Text is always ordinary text: a special token such as
// <|endoftext|> in a prompt is split into the ordinary tokens of its
// characters, which is how the OpenAI API treats it in message content, where
// tiktoken's default encode would refuse it. Each invalid UTF-8 byte is read as
// the symbol U+FFFD, since tiktoken has no answer for it either.
//
// The count is exact except for one case. A single unbroken piece of more than
// 256 KiB, which no text a person wrote contains, is merged in chunks and
// counted for the chunks, which is approximate.
func (e *Encoding) Count(text string) (int, error) {
	n, _, err := e.countPrefix(text, len(text))
	return n, err
}

// Encode returns the token ids of text, which Count counts. A piece of more
// than 256 KiB is merged in chunks here too, so its ids are approximate in the
// way its count is.
func (e *Encoding) Encode(text string) ([]uint32, error) {
	if err := e.load(); err != nil {
		return nil, err
	}
	var ids []uint32
	for i := 0; i < len(text); {
		end := e.scan(e.classes, text, i)
		ids = e.vocab.appendPiece(ids, text[i:end])
		i = end
	}
	return ids, nil
}

// countPrefix counts the pieces of text that end within the first limit bytes,
// and returns their tokens with the number of bytes they cover. The prefix
// always ends on a piece boundary, so its count is exact; it stops short of
// limit when the next piece would cross it.
func (e *Encoding) countPrefix(text string, limit int) (tokens, covered int, err error) {
	if err := e.load(); err != nil {
		return 0, 0, err
	}
	for covered < len(text) {
		end := e.scan(e.classes, text, covered)
		if end > limit {
			break
		}
		tokens += e.vocab.countPiece(text[covered:end])
		covered = end
	}
	return tokens, covered, nil
}

// Preload parses both encodings now instead of on first use, for a process
// that would rather pay for them at startup than on a request. It is safe to
// call more than once.
func Preload() error {
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		if err := e.load(); err != nil {
			return err
		}
	}
	return nil
}
