package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type logLine struct {
	Level   string            `json:"level"`
	Message string            `json:"msg"`
	Attrs   map[string]string `json:"plugin_attrs"`
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

func TestOutputRedactsSecretsAsWrittenAndAsJSON(t *testing.T) {
	var logged bytes.Buffer
	secret := `sk-"quoted"<secret>`
	o := newOutput(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), []string{secret, "sk-", ""})
	escaped, _ := json.Marshal(secret)
	o.record(abi.LogRecord{Level: "debug", Message: "raw " + secret, Attrs: map[string]string{"body": string(escaped)}})
	lines := logLines(t, &logged)
	if lines[0].Level != "DEBUG" || lines[0].Message != "raw [REDACTED]" || lines[0].Attrs["body"] != `"[REDACTED]"` {
		t.Fatalf("unredacted record %+v", lines[0])
	}
}

func TestOutputBoundsWhatACallLogs(t *testing.T) {
	var logged bytes.Buffer
	o := newOutput(slog.New(slog.NewJSONHandler(&logged, nil)), nil)
	attrs := map[string]string{}
	for i := range maxLogAttrs + 4 {
		attrs[fmt.Sprintf("k%02d", i)] = "v"
	}
	o.record(abi.LogRecord{Message: strings.Repeat("é", maxLogText), Attrs: attrs})
	lines := logLines(t, &logged)
	if len(lines[0].Attrs) != maxLogAttrs || !strings.HasSuffix(lines[0].Message, "é…") || len(lines[0].Message) > maxLogText+len("…") {
		t.Fatalf("record was not bounded: %d attributes, %d bytes", len(lines[0].Attrs), len(lines[0].Message))
	}
	for range maxCallLog / maxLogText {
		o.record(abi.LogRecord{Message: strings.Repeat("x", maxLogText)})
	}
	o.record(abi.LogRecord{Message: "after the budget"})
	lines = logLines(t, &logged)
	if last := lines[len(lines)-1]; last.Level != "WARN" || !strings.Contains(last.Message, "budget") || len(lines) > maxCallLog/maxLogText+1 {
		t.Fatalf("a call logged past its budget: %d records, last %q", len(lines), last.Message)
	}
}

func TestOutputStreamsLogOneRecordPerLine(t *testing.T) {
	var logged bytes.Buffer
	o := newOutput(slog.New(slog.NewJSONHandler(&logged, nil)), []string{"sk-split"})
	stderr := o.stream("stderr")
	for _, part := range []string{"first sk-", "split line\nsecond", " line\n\nunfinished"} {
		stderr.Write([]byte(part))
	}
	stderr.Write([]byte("\n" + strings.Repeat("y", maxCallLog+1)))
	stderr.Write([]byte("dropped sk-split\nlast"))
	o.close()
	var messages []string
	for _, line := range logLines(t, &logged) {
		if line.Attrs["stream"] != "stderr" {
			t.Fatalf("record %+v lacks its stream", line)
		}
		messages = append(messages, line.Message)
	}
	want := []string{"first [REDACTED] line", "second line", "unfinished", strings.Repeat("y", maxLogText) + "…", "last"}
	if strings.Join(messages, "|") != strings.Join(want, "|") {
		t.Fatalf("stream records %q, want %q", messages, want)
	}
}
