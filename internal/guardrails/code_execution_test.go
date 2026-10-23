package guardrails

import (
	"errors"
	"strings"
	"testing"
)

func TestCodeExecutionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, config, phase, text string
		blocked                   bool
	}{
		{"input execution", `{}`, Input, "Run this code:\n```py\nprint(1)\n```", true},
		{"explanation", `{}`, Input, "Explain:\n```py\nprint(1)\n```", false},
		{"negated execution", `{}`, Input, "Don't execute:\n```py\nprint(1)\n```", false},
		{"plain execution", `{}`, Input, "Execute this program", true},
		{"output code", `{}`, Output, "```python\nprint(1)\n```", true},
		{"CRLF output code", `{}`, Output, "```python\r\nprint(1)\r\n```", true},
		{"escaped output code", `{}`, Output, "```python\\nprint(1)\\n```", true},
		{"C++ output code", `{}`, Output, "```c++\nint main() {}\n```", true},
		{"C# output code", `{}`, Output, "```c#\nConsole.WriteLine(1);\n```", true},
		{"escaped C++ output code", `{}`, Output, "```c++\\nint main() {}\\n```", true},
		{"info string metadata", `{"blocked_languages":["python"]}`, Output, "```python title=example.py\nprint(1)\n```", true},
		{"punctuated language filter", `{"blocked_languages":["c++"]}`, Output, "```c++\nint main() {}\n```", true},
		{"language alias", `{"blocked_languages":["python"]}`, Output, "```py\nprint(1)\n```", true},
		{"other language", `{"blocked_languages":["python"]}`, Output, "```js\n1+1\n```", false},
		{"low confidence", `{"confidence_threshold":0.8}`, Output, "```text\nhello\n```", false},
		{"intent disabled", `{"detect_execution_intent":false}`, Input, "Explain:\n```py\nprint(1)\n```", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := testEngine(t, "builtin.code_execution", tc.config, tc.phase, Block, false, nil)
			result, err := evaluateText(t, engine, tc.text, tc.phase, nil)
			if result.Blocked != tc.blocked || errors.Is(err, ErrBlocked) != tc.blocked {
				t.Fatalf("blocked=%v error=%v", result.Blocked, err)
			}
		})
	}
}

func TestCodeExecutionRedaction(t *testing.T) {
	for _, tc := range []struct{ name, newline string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"escaped", `\n`}} {
		t.Run(tc.name, func(t *testing.T) {
			engine := testEngine(t, "builtin.code_execution", `{}`, Output, Redact, false, nil)
			input := "é" + tc.newline + "```c++ title=example.cpp" + tc.newline + "secret" + tc.newline + "```" + tc.newline + "終"
			want := "é" + tc.newline + "[CODE BLOCKED]" + tc.newline + "終"
			result, err := evaluateText(t, engine, input, Output, nil)
			if err != nil || len(result.Segments) != 1 || result.Segments[0].Text != want || !result.Changed {
				t.Fatalf("invalid fenced redaction: %+v %v", result, err)
			}
			if strings.Contains(result.Segments[0].Text, "secret") {
				t.Fatal("code was retained")
			}
		})
	}
}
