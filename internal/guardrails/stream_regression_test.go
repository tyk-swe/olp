package guardrails

import (
	"errors"
	"strings"
	"testing"
)

func TestStreamKeywordWaitsForWordBoundary(t *testing.T) {
	for _, action := range []Action{Block, Redact} {
		for _, suffix := range []string{"alogue", "é", "_name", "1", ""} {
			t.Run(string(action)+"/"+suffix, func(t *testing.T) {
				engine := testEngine(t, "builtin.keywords", `{"words":["cat"]}`, OutputStream, action, false, nil)
				stream, err := NewStream(engine, Evaluation{}, 1024)
				if err != nil {
					t.Fatal(err)
				}
				stream.SegmentRewriter(func(_ []byte, texts map[string]string) ([]byte, error) { return []byte(texts["stream"]), nil })
				if out, err := stream.PushSegments(t.Context(), []byte("cat"), []Delta{{ID: "stream", Text: "cat"}}); err != nil || len(out) != 0 {
					t.Fatalf("acted on an unconfirmed word boundary: %q %v", out, err)
				}
				var output strings.Builder
				if suffix != "" {
					out, err := stream.PushSegments(t.Context(), []byte(suffix), []Delta{{ID: "stream", Text: suffix}})
					if err != nil {
						t.Fatal(err)
					}
					for _, frame := range out {
						output.Write(frame)
					}
				}
				out, err := stream.Finish(t.Context())
				if suffix == "" && action == Block {
					if !errors.Is(err, ErrBlocked) {
						t.Fatalf("did not block final word: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, frame := range out {
					output.Write(frame)
				}
				want := "cat" + suffix
				if suffix == "" {
					want = "[REDACTED]"
				}
				if output.String() != want {
					t.Fatalf("got %q, want %q", output.String(), want)
				}
			})
		}
	}
	engine := testEngine(t, "builtin.keywords", `{"words":["cat"]}`, OutputStream, Block, false, nil)
	stream, _ := NewStream(engine, Evaluation{}, 1024)
	_, _ = stream.PushSegments(t.Context(), []byte("cat"), []Delta{{ID: "stream", Text: "cat"}})
	if _, err := stream.PushSegments(t.Context(), []byte(" "), []Delta{{ID: "stream", Text: " "}}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("did not block a confirmed word: %v", err)
	}
}
