package codexfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type Journey struct {
	mu        sync.Mutex
	mode      string
	turns     map[string]int
	sequence  int
	childDone chan struct{}
	childOnce sync.Once
}

func NewJourney(mode string) *Journey {
	return &Journey{mode: mode, turns: map[string]int{}, childDone: make(chan struct{})}
}

func (j *Journey) Reply(ctx context.Context, headers http.Header, body []byte) [][]byte {
	var request struct {
		Generate *bool `json:"generate"`
		Input    []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	_ = json.Unmarshal(body, &request)
	j.mu.Lock()
	j.sequence++
	id := fmt.Sprintf("resp-journey-%d", j.sequence)
	prewarm := request.Generate != nil && !*request.Generate
	thread := headers.Get("Thread-Id")
	if !prewarm {
		j.turns[thread]++
	}
	turn := j.turns[thread]
	j.mu.Unlock()
	if prewarm {
		return [][]byte{[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q}}`, id))}
	}
	item := `{"type":"message","role":"assistant","id":"msg-journey","content":[{"type":"output_text","text":"OLP journey complete"}]}`
	total := 16
	switch j.mode {
	case "tools":
		if turn == 1 {
			item = `{"type":"function_call","call_id":"call-journey","name":"exec_command","arguments":"{\"cmd\":\"printf OLP_CONTROLLED_TOOL\",\"max_output_tokens\":100}"}`
		}
	case "children":
		if headers.Get("X-Codex-Parent-Thread-Id") != "" {
			j.childOnce.Do(func() { close(j.childDone) })
		} else if turn == 1 {
			item = `{"type":"function_call","call_id":"spawn-journey","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"Inspect only this controlled fixture.\",\"task_name\":\"controlled_child\"}"}`
		} else {
			select {
			case <-j.childDone:
			case <-ctx.Done():
			}
		}
	case "compact":
		if turn == 1 {
			total = 20004
		}
		for _, input := range request.Input {
			if input.Type == "compaction_trigger" {
				item = `{"type":"compaction","encrypted_content":"OLP_CONTROLLED_COMPACT"}`
			}
		}
	}
	return [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id)),
		[]byte(`{"type":"response.output_item.done","item":` + item + `}`),
		[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"usage":{"input_tokens":%d,"output_tokens":4,"total_tokens":%d}}}`, id, total-4, total)),
	}
}
