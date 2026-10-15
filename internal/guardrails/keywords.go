package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

type KeywordConfig struct {
	Words       []string `json:"words"`
	Substrings  bool     `json:"substrings,omitempty"`
	Replacement string   `json:"replacement,omitempty"`
}
type keywordNode struct {
	next    map[rune]int
	fail    int
	lengths []int
}
type keywordDetector struct {
	nodes    []keywordNode
	config   KeywordConfig
	maxWidth int
	maxRunes int
}

func (k *keywordDetector) width() int { return k.maxWidth }
func compileKeywords(raw json.RawMessage) (*keywordDetector, error) {
	var config KeywordConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	if len(config.Words) < 1 || len(config.Words) > 1000 || len(config.Replacement) > 512 {
		return nil, errors.New("invalid keyword configuration")
	}
	if config.Replacement == "" {
		config.Replacement = "[REDACTED]"
	}
	k := &keywordDetector{config: config, nodes: []keywordNode{{next: map[rune]int{}}}}
	total := 0
	for _, word := range config.Words {
		total += len(word)
		if word == "" || len(word) > 512 || total > 16<<10 || !utf8.ValidString(word) {
			return nil, errors.New("invalid keyword list")
		}
		runes := []rune(cases.Fold().String(word))
		state := 0
		k.maxWidth = max(k.maxWidth, len(runes)*utf8.UTFMax+2*utf8.UTFMax)
		k.maxRunes = max(k.maxRunes, len(runes))
		for _, r := range runes {
			next, ok := k.nodes[state].next[r]
			if !ok {
				next = len(k.nodes)
				k.nodes = append(k.nodes, keywordNode{next: map[rune]int{}})
				k.nodes[state].next[r] = next
			}
			state = next
		}
		k.nodes[state].lengths = append(k.nodes[state].lengths, len(runes))
	}
	queue := []int{}
	for _, next := range k.nodes[0].next {
		queue = append(queue, next)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		for r, next := range k.nodes[state].next {
			queue = append(queue, next)
			f := k.nodes[state].fail
			for f != 0 && k.nodes[f].next[r] == 0 {
				f = k.nodes[f].fail
			}
			k.nodes[next].fail = k.nodes[f].next[r]
			k.nodes[next].lengths = append(k.nodes[next].lengths, k.nodes[k.nodes[next].fail].lengths...)
		}
	}
	return k, nil
}
func (k *keywordDetector) detect(ctx context.Context, e Evaluation) (Verdict, error) {
	v := Verdict{Action: "pass"}
	// Retain only the source starts a match can reference. ASCII folding does
	// no allocation; full Unicode folding uses reusable stack buffers.
	starts := make([]int, k.maxRunes+1)
	fold := cases.Fold()
	var source [utf8.UTFMax]byte
	var folded [32]byte
	for _, s := range e.Segments {
		state, position := 0, 0
		lastCheck := -1024
		consume := func(r rune, offset, end int, wholeEnd bool) error {
			starts[position%len(starts)] = offset
			for state != 0 && k.nodes[state].next[r] == 0 {
				state = k.nodes[state].fail
			}
			state = k.nodes[state].next[r]
			for _, length := range k.nodes[state].lengths {
				start := starts[(position-length+1)%len(starts)]
				if !k.config.Substrings {
					if !wholeEnd || position >= length && starts[(position-length)%len(starts)] == start {
						continue
					}
					before, _ := utf8.DecodeLastRuneInString(s.Text[:start])
					after, _ := utf8.DecodeRuneInString(s.Text[end:])
					if wordRune(before) || wordRune(after) {
						continue
					}
				}
				v.Action = Redact
				if len(v.Categories) == 0 {
					v.Categories = []string{"keyword.match"}
				}
				v.Redactions = append(v.Redactions, Redaction{s.ID, start, end, k.config.Replacement})
				if len(v.Redactions) > MaxFindings {
					return errEvaluation
				}
			}
			position++
			return nil
		}
		for offset, r := range s.Text {
			if offset-lastCheck >= 1024 {
				lastCheck = offset
				if err := ctx.Err(); err != nil {
					return v, err
				}
			}
			if r < utf8.RuneSelf {
				if r >= 'A' && r <= 'Z' {
					r += 'a' - 'A'
				}
				if err := consume(r, offset, offset+1, true); err != nil {
					return v, err
				}
				continue
			}
			n := utf8.EncodeRune(source[:], r)
			fold.Reset()
			size, read, err := fold.Transform(folded[:], source[:n], true)
			if err != nil || read != n {
				return v, errEvaluation
			}
			for i := 0; i < size; {
				foldedRune, width := utf8.DecodeRune(folded[i:size])
				if err := consume(foldedRune, offset, offset+n, i+width == size); err != nil {
					return v, err
				}
				i += width
			}
		}
	}
	v.Redactions = mergeRedactions(v.Redactions)
	return v, nil
}
