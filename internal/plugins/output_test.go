package plugins

import (
	"bytes"
	"encoding/json"
	"testing"
)

type logLine struct {
	Level   string            `json:"level"`
	Message string            `json:"msg"`
	Attrs   map[string]string `json:"plugin_attrs"`
	Error   string            `json:"error"`
}

func logLines(t *testing.T, logged *bytes.Buffer) []logLine {
	t.Helper()
	var lines []logLine
	for _, raw := range bytes.Split(bytes.TrimSpace(logged.Bytes()), []byte("\n")) {
		var line logLine
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return lines
}

// budgetSpent adds up what the records in logged cost their call's budget and
// counts the warnings that output was dropped.
func budgetSpent(t *testing.T, logged *bytes.Buffer) (spent, warnings int) {
	t.Helper()
	for _, line := range logLines(t, logged) {
		if line.Level == "WARN" {
			warnings++
			continue
		}
		spent += recordCost + len(line.Message)
		for key, value := range line.Attrs {
			spent += len(key) + len(value)
		}
	}
	return spent, warnings
}
