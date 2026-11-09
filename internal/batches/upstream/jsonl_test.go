package upstream

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/batches"
)

func TestReadPagesResumesFromCheckpointWithoutDuplicates(t *testing.T) {
	lines := make([][]byte, 2*batches.PageSize+50)
	want := make([]string, len(lines))
	for i := range lines {
		want[i] = fmt.Sprintf("request-%d", i)
		lines[i] = fmt.Appendf(nil, `{"custom_id":%q,"response":{"status_code":200,"body":{"id":"result","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}}}`+"\n", want[i])
	}
	input := bytes.Join(lines, nil)
	errStop := errors.New("stop")
	read := func(body []byte, from batches.Cursor, commitLimit int) ([]string, batches.Cursor, batches.Cursor, error) {
		var committed []string
		var checkpoint batches.Cursor
		commits := 0
		final, _, _, err := readPages(bytes.NewReader(body), from, "file-out", batches.PhaseOutput,
			func(line []byte, _ int64) (batches.Result, error) {
				return ParseLine(line, "/v1/chat/completions", false)
			},
			func(page []batches.Result, cursor batches.Cursor) error {
				if commits == commitLimit {
					return errStop
				}
				commits++
				for _, result := range page {
					committed = append(committed, result.CustomID)
				}
				checkpoint = cursor
				return nil
			}, batches.MaxBatchBytes)
		return committed, checkpoint, final, err
	}

	first, checkpoint, _, err := read(input, batches.Cursor{}, 1)
	if !errors.Is(err, errStop) || len(first) != batches.PageSize {
		t.Fatalf("interrupted read committed %d results, err %v", len(first), err)
	}
	rest, _, final, err := read(input, checkpoint, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Concat(first, rest); !slices.Equal(got, want) {
		t.Fatalf("resumed read committed %d results, want each of %d exactly once", len(got), len(want))
	}
	if again, _, _, err := read(input, final, -1); err != nil || len(again) != 0 {
		t.Fatalf("rereading a finished file committed %d results, err %v", len(again), err)
	}

	tampered := bytes.Join(slices.Concat(lines[:3], [][]byte{bytes.Replace(lines[3], []byte(`"prompt_tokens":2`), []byte(`"prompt_tokens":9`), 1)}, lines[4:]), nil)
	for name, body := range map[string][]byte{"rewritten prefix": tampered, "truncated file": bytes.Join(lines[:50], nil)} {
		if committed, _, _, err := read(body, checkpoint, -1); !errors.Is(err, batches.ErrCursorConflict) || len(committed) != 0 {
			t.Errorf("%s: committed %d results, err %v, want cursor conflict", name, len(committed), err)
		}
	}
}
