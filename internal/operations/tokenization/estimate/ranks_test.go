package estimate

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var updateRanks = flag.Bool("update-ranks", false, "download the public rank files and rewrite ranks/")

// rankSources are OpenAI's public rank files and the SHA-256 that tiktoken
// 0.14.0 pins for each in tiktoken_ext/openai_public.py, checked against the
// bytes fetched from the URLs on 2026-10-01.
var rankSources = []struct {
	encoding *Encoding
	file     string
	url      string
	sha256   string
	tokens   int
}{
	{O200kBase, "ranks/o200k_base.bin", "https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken",
		"446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d", 199998},
	{CL100kBase, "ranks/cl100k_base.bin", "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken",
		"223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7", 100256},
}

// publicRanks rebuilds the public .tiktoken text, one "base64(token) rank"
// line per token, from a vocabulary.
func publicRanks(v *vocab) []byte {
	var out bytes.Buffer
	for r := range uint32(len(v.offsets) - 1) {
		out.WriteString(base64.StdEncoding.EncodeToString([]byte(v.token(r))))
		out.WriteByte(' ')
		out.WriteString(strconv.FormatUint(uint64(r), 10))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// compactRanks converts the public text into the embedded form, requiring each
// token's rank to be its line number, which the format relies on.
func compactRanks(public []byte) ([]byte, error) {
	var lengths, blob []byte
	count := 0
	for rank, line := range strings.Split(strings.TrimSuffix(string(public), "\n"), "\n") {
		token, number, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("line %d: no rank", rank)
		}
		if n, err := strconv.Atoi(number); err != nil || n != rank {
			return nil, fmt.Errorf("line %d: rank %q is not the line number", rank, number)
		}
		raw, err := base64.StdEncoding.DecodeString(token)
		if err != nil || len(raw) == 0 {
			return nil, fmt.Errorf("line %d: bad token %q", rank, token)
		}
		lengths = appendUvarint(lengths, uint64(len(raw)))
		blob = append(blob, raw...)
		count++
	}
	out := appendUvarint([]byte(rankMagic), uint64(count))
	return append(append(out, lengths...), blob...), nil
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// TestRankSources proves the embedded rank files are OpenAI's: each rebuilds,
// byte for byte, the public file whose hash tiktoken pins.
func TestRankSources(t *testing.T) {
	for _, s := range rankSources {
		t.Run(s.encoding.Name(), func(t *testing.T) {
			v, err := newVocab(s.encoding.ranks)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(v.offsets) - 1; n != s.tokens {
				t.Errorf("%d tokens, want %d", n, s.tokens)
			}
			sum := sha256.Sum256(publicRanks(v))
			if got := hex.EncodeToString(sum[:]); got != s.sha256 {
				t.Errorf("rebuilt public file hashes to %s, want %s", got, s.sha256)
			}
		})
	}
}

// TestUpdateRanks regenerates ranks/ from OpenAI's public files. It does
// nothing unless -update-ranks is given, since it needs the network, and
// refuses any download that does not hash to the pinned value. Run the suite
// again afterwards, because the embedded copy is read at build time.
func TestUpdateRanks(t *testing.T) {
	if !*updateRanks {
		t.Skip("pass -update-ranks to download the public rank files")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	for _, s := range rankSources {
		resp, err := client.Get(s.url)
		if err != nil {
			t.Fatal(err)
		}
		public, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, %v", s.url, resp.StatusCode, err)
		}
		sum := sha256.Sum256(public)
		if got := hex.EncodeToString(sum[:]); got != s.sha256 {
			t.Fatalf("%s hashes to %s, tiktoken pins %s", s.url, got, s.sha256)
		}
		compact, err := compactRanks(public)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.file, compact, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactRanksRoundTrip(t *testing.T) {
	public := "AA== 0\nAQ== 1\nAAE= 2\n"
	compact, err := compactRanks([]byte(public))
	if err != nil {
		t.Fatal(err)
	}
	// Three tokens, of lengths 1, 1 and 2.
	if want := rankMagic + "\x03\x01\x01\x02" + "\x00\x01\x00\x01"; string(compact) != want {
		t.Fatalf("compact form is %q, want %q", compact, want)
	}
	for _, bad := range []string{"AA== 1\n", "AA== 0\nAQ== 2\n", "AA==\n", "!!! 0\n", " 0\n"} {
		if _, err := compactRanks([]byte(bad)); err == nil {
			t.Errorf("compactRanks accepted %q", bad)
		}
	}
}

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

// TestVocabLookup checks the lookup structures against the token list they
// were built from, for every token and for near misses.
func TestVocabLookup(t *testing.T) {
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		t.Run(e.name, func(t *testing.T) {
			v, err := newVocab(e.ranks)
			if err != nil {
				t.Fatal(err)
			}
			longest := 0
			for r := range uint32(len(v.offsets) - 1) {
				token := v.token(r)
				longest = max(longest, len(token))
				if got := v.rank(token); got != r {
					t.Fatalf("rank(%q) = %d, want %d", token, got, r)
				}
				// A token with one more byte, or one fewer, is only a token if
				// the list says so.
				if got := v.rank(token + "\xff\xfe"); got != noRank && v.token(got) != token+"\xff\xfe" {
					t.Fatalf("rank(%q+junk) = %d", token, got)
				}
			}
			if v.longest != longest {
				t.Errorf("longest = %d, want %d", v.longest, longest)
			}
			if v.rank("") != noRank || v.rank(strings.Repeat("a", v.longest+1)) != noRank {
				t.Error("the empty string and an over-long string are tokens")
			}
			for b := range 256 {
				if r := v.rank(string([]byte{byte(b)})); v.token(r) != string([]byte{byte(b)}) {
					t.Errorf("byte %#x has token %q", b, v.token(r))
				}
			}
		})
	}
}
