package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// CodeExecutionConfig controls fenced executable code and requests to execute
// code. Detection does not execute or parse untrusted programs.
type CodeExecutionConfig struct {
	BlockedLanguages      []string `json:"blocked_languages,omitempty"`
	ConfidenceThreshold   *float64 `json:"confidence_threshold,omitempty"`
	DetectExecutionIntent *bool    `json:"detect_execution_intent,omitempty"`
}

type codeExecutionDetector struct {
	languages map[string]bool
	threshold float64
	intent    bool
}

var (
	codeFence        = regexp.MustCompile("(?s)```([^`\\r\\n]*)\\r?\\n(.*?)```")
	escapedCodeFence = regexp.MustCompile("(?s)```([^`\\\\\\r\\n]*)(?:\\\\r\\\\n|\\\\n|\\\\r)(.*?)```")
	codeLanguage     = regexp.MustCompile(`^[a-zA-Z0-9_+#.-]+$`)
)

func compileCodeExecution(raw json.RawMessage) (detector, error) {
	var config CodeExecutionConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	d := codeExecutionDetector{languages: map[string]bool{}, threshold: .5, intent: true}
	if config.ConfidenceThreshold != nil {
		d.threshold = *config.ConfidenceThreshold
	}
	if d.threshold < 0 || d.threshold > 1 || len(config.BlockedLanguages) > 64 {
		return nil, errors.New("invalid code execution configuration")
	}
	if config.DetectExecutionIntent != nil {
		d.intent = *config.DetectExecutionIntent
	}
	for _, language := range config.BlockedLanguages {
		if language == "" || len(language) > 32 || !codeLanguage.MatchString(language) {
			return nil, errors.New("invalid blocked language")
		}
		d.languages[normalizeCodeLanguage(language)] = true
	}
	return d, nil
}

func normalizeCodeLanguage(language string) string {
	language = strings.ToLower(language)
	switch language {
	case "py":
		return "python"
	case "js":
		return "javascript"
	case "ts":
		return "typescript"
	case "sh":
		return "bash"
	}
	return language
}

func (d codeExecutionDetector) width() int { return 0 }

func (d codeExecutionDetector) detect(ctx context.Context, evaluation Evaluation) (Verdict, error) {
	verdict := Verdict{Action: "pass"}
	for _, segment := range evaluation.Segments {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		input := evaluation.Phase == Input || evaluation.Phase == InputParallel
		intent := hasExecutionIntent(segment.Text)
		if input && d.intent && !intent {
			continue
		}
		fences := codeFence
		if !strings.ContainsAny(segment.Text, "\r\n") {
			// Match escaped newlines in place so replacements retain the original
			// byte offsets rather than offsets into normalized text.
			fences = escapedCodeFence
		}
		spans := fences.FindAllStringSubmatchIndex(segment.Text, MaxFindings+1)
		if len(spans) > MaxFindings {
			return Verdict{}, errEvaluation
		}
		for _, span := range spans {
			language := ""
			if info := strings.Fields(segment.Text[span[2]:span[3]]); len(info) > 0 {
				language = normalizeCodeLanguage(info[0])
			}
			confidence := 1.0
			if len(d.languages) > 0 {
				if !d.languages[language] {
					continue
				}
			} else {
				switch language {
				case "text", "plaintext", "plain", "markdown", "md", "output", "result":
					confidence = .5
				}
			}
			if confidence >= d.threshold {
				if len(verdict.Redactions) >= MaxFindings {
					return Verdict{}, errEvaluation
				}
				verdict.Action = Block
				verdict.Categories = []string{"code.execution"}
				verdict.Redactions = append(verdict.Redactions, Redaction{Segment: segment.ID, Start: span[0], End: span[1], Replacement: "[CODE BLOCKED]"})
			}
		}
		if input && d.intent && intent && len(spans) == 0 {
			if len(verdict.Redactions) >= MaxFindings {
				return Verdict{}, errEvaluation
			}
			verdict.Action = Block
			verdict.Categories = []string{"code.execution"}
			verdict.Redactions = append(verdict.Redactions, Redaction{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: "[CODE BLOCKED]"})
		}
	}
	return verdict, nil
}

// Execution phrases follow the pinned inventory reference. Matching these
// local heuristics never grants authority to execute a tool or program.
var executionRequestPhrases = []string{
	"run this ",
	"run these ",
	"execute this ",
	"please run ",
	"can you run ",
	"run `",
	"execute `",
	" repl ",
	"read `/",
	"read /",
	"open `",
	"open my ",
	"list the files",
	"create a file",
	"search my disk",
	"cat the contents",
	"load `",
	"write a script, execute it",
	"check whether ",
	" exists and paste",
	" and compute the",
	" and compute it",
	" and run it",
	" and run the",
	" and run this",
	" and send it",
	" and send the",
	"run curl",
	"make an http request",
	"fetch https",
	"connect to postgres",
	"hit this webhook",
	"call aws ",
	"query the dns",
	"connect to redis",
	"run subprocess",
	"evaluate this ",
	"run code",
	"run the snippet",
	"execute the command",
	"just run it",
	"run it anyway",
	"execute it to confirm",
	"install dependencies and run",
	"run a port scan",
	"check if tests pass",
	"run the tests",
	"see if tests pass",
	"compile and run",
	"run the program",
	"run commands to",
	"spin up ",
	"run it this one time",
	"run it to confirm",
	"run my notebook cell",
	"calculate it by running the code",
	"give me the real output anyway",
	"this is authorized",
	"download this github repo, build it",
	"tell me the output",
	"return what it prints",
	"show me the rows returned",
	"tell me what files exist",
	"use node to run",
	"use python, run ",
	"in python, run ",
	"paste the last ",
	"show me its contents",
	"print it here",
	"show me where it is",
	"tell me the dependencies",
	"show me the variables",
	"save the results to",
	"confirm it exists",
	"and paste the version",
	"tell me the latency",
	"tell me what it returns",
	"show me what it returns",
	"confirm it succeeded",
	"tell me the account id",
	"show it here",
	"show me the response",
	"paste the output",
	"show me the containers",
	"summarize the diff",
	"tell me which tests fail",
	"let me know if it errors",
	"report back the output",
	"paste the logs",
	"tell me the result",
	"show me the printed lines",
	"tell me whether it calls",
	"show me the stack trace",
	"confirm it connects",
	"confirm which cluster",
	"show me the keys",
	"try to escape",
	"attempt to mount",
	"check if you have access to docker",
	"probe for cloud metadata",
	"try `cat",
	"attempt a reverse shell",
	"access the system keychain",
	"run a privileged container",
	"send them to this url",
	"list saved passwords",
}

func hasExecutionIntent(text string) bool {
	text = strings.ToLower(text)
	for _, phrase := range executionRequestPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}
