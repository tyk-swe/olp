//go:build bench

package loadgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// shortPrompt is the prompt of a request whose size is 0 tokens.
const shortPrompt = "Reply with one short sentence."

// corpus is the vocabulary of the large prompts: common English words, each
// a single token with its leading space in the OpenAI encodings, so a prompt
// of N words is about N tokens whichever tokenizer reads it. The words are
// plain lowercase letters, so a prompt needs no JSON escaping.
const corpus = "the of and to in is you that it he was for on are as with his they at be this have from or one had by word but not what all were we when your can said there use an each which she do how their if will up other about out many then them these so some her would make like him into time has look two more write go see number no way could people my than first water been call who oil its now find long down day did get come made may part " +
	"over new sound take only little work know place year live me back give most very after thing our just name good sentence man think say great where help through much before line right too mean old any same tell boy follow came want show also around form three small set put end does another well large must big even such because turn here why ask went men read need land different home us move try kind hand picture again change off play spell air away animal house point page letter mother answer found study still learn should " +
	"world school state family student group country problem case week company system program question government night side head fact month lot book eye job business issue area money story young science few since second children begin seem next walk example life being between never together"

var vocabulary = strings.Fields(corpus)

// prompt returns a prompt of tokens words, drawn from the vocabulary by a
// fixed pseudo-random sequence so it is the same on every run and does not
// compress to nothing. Zero tokens is the short prompt.
func prompt(tokens int) []byte {
	if tokens <= 0 {
		return []byte(shortPrompt)
	}
	out := make([]byte, 0, tokens*6)
	state := uint64(0x9E3779B97F4A7C15)
	for i := 0; i < tokens; i++ {
		// xorshift64*
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		word := vocabulary[((state*0x2545F4914F6CDD1D)>>33)%uint64(len(vocabulary))]
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, word...)
	}
	return out
}

// bodies renders request bodies. Each request is three parts: a small head
// that names the request, the prompt, which is shared by every request of
// its size and never copied, and a small tail. The head carries a request
// number at the start of the prompt, so no two requests are byte-identical.
type bodies struct {
	cfg       Config
	modelJSON []byte
	prompts   [][]byte
	urls      [2]*url.URL // unary, streaming
}

func newBodies(cfg Config) (*bodies, error) {
	b := &bodies{cfg: cfg}
	var err error
	if b.modelJSON, err = json.Marshal(cfg.Model); err != nil {
		return nil, err
	}
	for _, n := range cfg.PromptTokens {
		b.prompts = append(b.prompts, prompt(n))
	}
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	for i, stream := range []bool{false, true} {
		u := *base
		p := cfg.Path
		if p == "" {
			switch cfg.Dialect {
			case OpenAI:
				p = "/v1/chat/completions"
			case Anthropic:
				p = "/v1/messages"
			case Gemini:
				p = "/v1beta/models/{model}:{operation}"
			}
		}
		operation := "generateContent"
		if stream {
			operation = "streamGenerateContent"
		}
		p = strings.NewReplacer("{model}", url.PathEscape(cfg.Model), "{operation}", operation).Replace(p)
		rawPath := strings.TrimRight(base.EscapedPath(), "/") + "/" + strings.TrimLeft(p, "/")
		if u.Path, err = url.PathUnescape(rawPath); err != nil {
			return nil, fmt.Errorf("path %q: %w", p, err)
		}
		u.RawPath = rawPath
		if cfg.Dialect == Gemini && stream {
			u.RawQuery = "alt=sse"
		}
		b.urls[i] = &u
	}
	return b, nil
}

// streamsBefore is how many of the requests before k stream, and ppm the
// stream share in parts per million.
func (b *bodies) streamsBefore(k int64) (n, ppm int64) {
	ppm = int64(b.cfg.StreamShare*1e6 + 0.5)
	return k * ppm / 1_000_000, ppm
}

// streams reports whether request k streams: a share spread as evenly as
// integers allow, so every window of requests holds the same mix.
func (b *bodies) streams(k int64) bool {
	before, ppm := b.streamsBefore(k)
	return (k+1)*ppm/1_000_000 > before
}

// size is which prompt request k uses: equal shares, in rotation. The rotation
// runs over the requests of k's own mode, streaming or not, so that the sizes
// and the modes stay independent of each other: rotating over k itself would
// give an even number of sizes and an even stream share one size for each
// mode, and the latencies of the two modes would differ by prompt as much as
// by mode.
func (b *bodies) size(k int64) int {
	before, _ := b.streamsBefore(k)
	if !b.streams(k) {
		before = k - before
	}
	return int(before % int64(len(b.prompts)))
}

const bodyTail = `"}]`

// request builds request k.
func (b *bodies) request(k int64) (*http.Request, int64, error) {
	stream := b.streams(k)
	var head []byte
	tail := bodyTail
	model := b.modelJSON
	switch b.cfg.Dialect {
	case OpenAI:
		head = append(head, `{"model":`...)
		head = append(head, model...)
		head = append(head, `,"stream":`...)
		head = strconv.AppendBool(head, stream)
		head = append(head, `,"max_tokens":`...)
		head = strconv.AppendInt(head, int64(b.cfg.MaxTokens), 10)
		head = append(head, `,"messages":[{"role":"user","content":"`...)
		if stream {
			tail += `,"stream_options":{"include_usage":true}`
		}
		tail += `}`
	case Anthropic:
		head = append(head, `{"model":`...)
		head = append(head, model...)
		head = append(head, `,"max_tokens":`...)
		head = strconv.AppendInt(head, int64(b.cfg.MaxTokens), 10)
		head = append(head, `,"stream":`...)
		head = strconv.AppendBool(head, stream)
		head = append(head, `,"messages":[{"role":"user","content":"`...)
		tail += `}`
	case Gemini:
		head = append(head, `{"contents":[{"role":"user","parts":[{"text":"`...)
		tail = `"}]}],"generationConfig":{"maxOutputTokens":` + strconv.Itoa(b.cfg.MaxTokens) + `}}`
	}
	head = append(head, "request "...)
	head = strconv.AppendInt(head, k, 10)
	head = append(head, ": "...)
	body := b.prompts[b.size(k)]
	total := int64(len(head) + len(body) + len(tail))
	newBody := func() io.ReadCloser {
		return io.NopCloser(io.MultiReader(bytes.NewReader(head), bytes.NewReader(body), strings.NewReader(tail)))
	}

	u := b.urls[0]
	if stream {
		u = b.urls[1]
	}
	req := &http.Request{Method: http.MethodPost, URL: u, Host: u.Host, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header, 6), Body: newBody(), ContentLength: total}
	req.GetBody = func() (io.ReadCloser, error) { return newBody(), nil }
	h := req.Header
	h.Set("Content-Type", "application/json")
	switch b.cfg.Dialect {
	case OpenAI:
		if b.cfg.APIKey != "" {
			h.Set("Authorization", "Bearer "+b.cfg.APIKey)
		}
	case Anthropic:
		h.Set("Anthropic-Version", "2023-06-01")
		if b.cfg.APIKey != "" {
			h.Set("X-Api-Key", b.cfg.APIKey)
		}
	case Gemini:
		if b.cfg.APIKey != "" {
			h.Set("X-Goog-Api-Key", b.cfg.APIKey)
		}
	}
	if stream {
		h.Set("Accept", "text/event-stream")
	}
	for name, value := range b.cfg.Headers {
		h.Set(name, value)
	}
	return req, total, nil
}
