package estimate

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// corpus returns the named oracle fixtures' texts, joined.
func corpus(tb testing.TB, names ...string) string {
	tb.Helper()
	byName := map[string]string{}
	for _, f := range loadFixtures(tb, "o200k_base.json") {
		byName[f.Name] = f.Text
	}
	var parts []string
	for _, name := range names {
		text, ok := byName[name]
		if !ok {
			tb.Fatalf("no fixture %q", name)
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

// promptOf repeats the corpus until encoding it takes at least tokens tokens.
func promptOf(tb testing.TB, e *Encoding, seed string, tokens int) string {
	tb.Helper()
	one, err := e.Count(seed)
	if err != nil || one == 0 {
		tb.Fatalf("seed corpus: %d tokens, %v", one, err)
	}
	return strings.Repeat(seed+"\n", tokens/one+1)
}

var benchPrompts = []struct {
	name  string
	names []string
}{
	{"prose", []string{"document-52k"}},
	{"code", []string{"code-go", "code-python", "code-javascript"}},
	{"json", []string{"tools-pretty", "json-pretty", "tools-compact"}},
	{"multilingual", []string{"multilingual-chinese", "multilingual-japanese", "multilingual-hindi", "multilingual-arabic", "multilingual-russian", "multilingual-mixed-scripts"}},
}

// BenchmarkLoad is the first-use cost of an encoding: parsing its rank file
// and building the lookup table.
func BenchmarkLoad(b *testing.B) {
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		b.Run(e.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := newVocab(e.ranks); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCount measures exact counting of prompts of the sizes the
// high-throughput profile of the milestone names, 50K and 100K tokens.
func BenchmarkCount(b *testing.B) {
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		for _, p := range benchPrompts {
			for _, tokens := range []int{50_000, 100_000} {
				seed := corpus(b, p.names...)
				text := promptOf(b, e, seed, tokens)
				b.Run(e.name+"/"+p.name+"/"+itoa(tokens), func(b *testing.B) {
					b.SetBytes(int64(len(text)))
					b.ReportAllocs()
					for range b.N {
						if _, err := e.Count(text); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// BenchmarkMeter is what admission pays: a request's text through the
// bounded policy, so a long prompt costs the exact part plus arithmetic.
func BenchmarkMeter(b *testing.B) {
	for _, model := range []string{"gpt-4o", "gpt-4"} {
		c := ForModel(model)
		for _, p := range benchPrompts {
			for _, tokens := range []int{50_000, 100_000} {
				text := promptOf(b, c.encoding, corpus(b, p.names...), tokens)
				b.Run(model+"/"+p.name+"/"+itoa(tokens), func(b *testing.B) {
					b.SetBytes(int64(len(text)))
					b.ReportAllocs()
					for range b.N {
						m := c.Meter()
						m.Add(text)
						m.Total()
					}
				})
			}
		}
	}
}

// BenchmarkHeuristic is the cost of the families without a tokenizer.
func BenchmarkHeuristic(b *testing.B) {
	c := ForModel("claude-sonnet-4-5")
	text := promptOf(b, O200kBase, corpus(b, "document-52k"), 100_000)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for range b.N {
		m := c.Meter()
		m.Add(text)
		m.Total()
	}
}

// BenchmarkUnbrokenPieces is the shape of input that makes a naive merge
// quadratic: one piece of a hundred thousand bytes.
func BenchmarkUnbrokenPieces(b *testing.B) {
	rng := rand.New(rand.NewPCG(11, 12))
	letters := make([]byte, 100_000)
	for i := range letters {
		letters[i] = "abcdefghijklmnopqrstuvwxyz"[rng.IntN(26)]
	}
	for name, text := range map[string]string{
		"spaces":  strings.Repeat(" ", 100_000),
		"a":       strings.Repeat("a", 100_000),
		"equals":  strings.Repeat("=", 100_000),
		"cjk":     strings.Repeat("\U00004eca\U00005929\U00005929\U00006c14", 100_000/12),
		"letters": string(letters),
		"newline": strings.Repeat("\n", 100_000),
	} {
		for _, e := range []*Encoding{O200kBase, CL100kBase} {
			b.Run(e.name+"/"+name, func(b *testing.B) {
				b.SetBytes(int64(len(text)))
				b.ReportAllocs()
				for range b.N {
					if _, err := e.Count(text); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkMeterWorstCase is the most one request can cost admission: text
// that is a single piece just inside the exact bound, which only the heap
// merge handles and which is the slowest per byte.
func BenchmarkMeterWorstCase(b *testing.B) {
	rng := rand.New(rand.NewPCG(13, 14))
	letters := make([]byte, ExactBytes-1)
	for i := range letters {
		letters[i] = "abcdefghijklmnopqrstuvwxyz"[rng.IntN(26)]
	}
	c := ForModel("gpt-4o")
	for name, text := range map[string]string{
		"spaces":  strings.Repeat(" ", ExactBytes-1),
		"letters": string(letters),
		"a":       strings.Repeat("a", ExactBytes-1),
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for range b.N {
				m := c.Meter()
				m.Add(text)
				m.Total()
			}
		})
	}
}

// BenchmarkEstimate is what counting a short prompt costs for each family, which
// is the work admission does for most requests: the meter made, the text added and
// the total taken, with the exact bound of ExactBytes in force. The families with
// a tokenizer count the prompt exactly and the rest charge four characters to a
// token. The prompts of 50K and 100K tokens are measured by BenchmarkMeter for the
// families with a tokenizer, and by BenchmarkHeuristic for the rest, which share
// one rule and differ only by the factor of each.
//
//	go test ./internal/operations/tokenization/estimate -run '^$' -bench BenchmarkEstimate -benchmem
func BenchmarkEstimate(b *testing.B) {
	prompt := corpus(b, "prose-2")
	for _, family := range []struct {
		name, model string
		family      Family
	}{
		{"openai-o200k", "gpt-4o", FamilyOpenAIO200k},
		{"openai-cl100k", "gpt-4", FamilyOpenAICL100k},
		{"anthropic", "claude-sonnet-4-5", FamilyAnthropic},
		{"gemini", "gemini-2.5-pro", FamilyGemini},
		{"other", "llama-3.3-70b-instruct", FamilyOther},
	} {
		b.Run(family.name, func(b *testing.B) {
			counter := ForModel(family.model)
			want := ProvenanceHeuristic
			if counter.encoding != nil {
				want = ProvenanceTokenizer
			}
			if _, provenance := counter.Count(prompt); counter.Family() != family.family || provenance != want {
				b.Fatalf("%s is in family %s and counted as %s, want %s and %s: the case no longer measures what it names", family.model, counter.Family(), provenance, family.family, want)
			}
			b.SetBytes(int64(len(prompt)))
			b.ReportAllocs()
			for b.Loop() {
				m := counter.Meter()
				m.Add(prompt)
				m.Total()
			}
		})
	}
}
