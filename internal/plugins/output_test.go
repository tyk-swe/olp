package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
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

func TestOutputRedactsEncodedCredentials(t *testing.T) {
	secret := `sk-"quoted"<secret>&+/= end`
	var plainJSON bytes.Buffer
	encoder := json.NewEncoder(&plainJSON)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]string{"token": secret}); err != nil {
		t.Fatal(err)
	}
	for name, message := range map[string]string{
		"query":                      "https://authority.example/token?token=" + url.QueryEscape(secret),
		"path":                       "https://authority.example/token/" + url.PathEscape(secret),
		"JSON without HTML escaping": strings.TrimSpace(plainJSON.String()),
	} {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			o := newOutput(slog.New(slog.NewJSONHandler(&logged, nil)), []string{secret})
			o.record(abi.LogRecord{Message: message, Attrs: map[string]string{message: message}})
			line := logLines(t, &logged)[0]
			if !strings.Contains(line.Message, "[REDACTED]") || line.Message == message {
				t.Fatal("the log message still contains the encoded credential")
			}
			if line.Attrs[line.Message] != line.Message {
				t.Fatal("attribute names and values were not redacted together")
			}
			err := reportedFailure(o, abi.Response{Error: &abi.Error{Code: message, Message: message}})
			if strings.Contains(err.Error(), message) || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatal("the reported failure still contains the encoded credential")
			}
		})
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
	want := []string{"first [REDACTED] line", "second line", "unfinished", "last"}
	if strings.Join(messages, "|") != strings.Join(want, "|") {
		t.Fatalf("stream records %q, want %q", messages, want)
	}
}

// Every record costs the call's budget, so a plugin can't log without bound
// by logging empty records.
func TestOutputChargesEveryRecord(t *testing.T) {
	var logged bytes.Buffer
	o := newOutput(slog.New(slog.NewJSONHandler(&logged, nil)), nil)
	for range 10 * maxCallLog {
		o.record(abi.LogRecord{})
	}
	if lines := logLines(t, &logged); len(lines) > maxCallLog/recordCost+1 {
		t.Fatalf("a call logged %d empty records", len(lines))
	}
}

// An unconfined plugin's capability requests for one call are served
// concurrently, and all of them spend the call's one budget.
func TestOutputBudgetHoldsForConcurrentRecords(t *testing.T) {
	var logged bytes.Buffer
	o := newOutput(slog.New(slog.NewJSONHandler(&logged, nil)), nil)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 64 {
				o.record(abi.LogRecord{Message: strings.Repeat("x", 100)})
			}
		})
	}
	wg.Wait()
	spent, warnings := budgetSpent(t, &logged)
	if spent > maxCallLog || warnings != 1 {
		t.Fatalf("concurrent records spent %d bytes of a %d byte budget, with %d warnings", spent, maxCallLog, warnings)
	}
}
