//go:build integration

package gosdk

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Compile and execute the operator's generated constructors with the same
// pinned SDKs as the rest of this module, including an actual gateway request.
func TestOperatorGeneratedGoClients(t *testing.T) {
	for _, surface := range []string{"openai", "anthropic", "gemini"} {
		t.Run(surface, func(t *testing.T) {
			h := connect(t)
			binary := os.Getenv("OLP_CLIENTS_OPERATOR_BINARY")
			if binary == "" {
				t.Fatal("run through tests/clients/run.sh")
			}
			dir := t.TempDir()
			keyFile := filepath.Join(dir, "mounted key")
			if err := os.WriteFile(keyFile, []byte(h.apiKey+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			source, err := exec.CommandContext(t.Context(), binary, "client-env", surface, "--format", "go", "--url", h.origin, "--model", h.models[surface], "--key-file", keyFile).Output()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(source), h.apiKey) {
				t.Fatal("key entered generated source")
			}
			config := filepath.Join(dir, "config.go")
			if err := os.WriteFile(config, source, 0600); err != nil {
				t.Fatal(err)
			}
			var imports, request string
			switch surface {
			case "openai":
				imports = `"github.com/openai/openai-go/v3"`
				request = `result,err:=client.Chat.Completions.New(ctx,openai.ChatCompletionNewParams{Model:Model,Messages:[]openai.ChatCompletionMessageParamUnion{openai.UserMessage("Say hello.")},MaxCompletionTokens:openai.Int(64)});if err!=nil{t.Fatal(err)};answer=result.Choices[0].Message.Content`
			case "anthropic":
				imports = `"github.com/anthropics/anthropic-sdk-go"`
				request = `result,err:=client.Messages.New(ctx,anthropic.MessageNewParams{Model:anthropic.Model(Model),MaxTokens:64,Messages:[]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say hello."))}});if err!=nil{t.Fatal(err)};answer=result.Content[0].Text`
			case "gemini":
				imports = `"google.golang.org/genai"`
				request = `result,err:=client.Models.GenerateContent(ctx,Model,genai.Text("Say hello."),&genai.GenerateContentConfig{MaxOutputTokens:64});if err!=nil{t.Fatal(err)};answer=result.Text()`
			}
			testSource := fmt.Sprintf("package olpclient\nimport(\"context\";\"testing\";\"time\";%s)\nfunc TestRequest(t *testing.T){ctx,cancel:=context.WithTimeout(context.Background(),20*time.Second);defer cancel();client,err:=New(ctx);if err!=nil{t.Fatal(err)};var answer string;%s;if answer!=%q{t.Fatalf(\"unexpected reply: %%s\",answer)}}", imports, request, h.defaultReply)
			generatedTest := filepath.Join(dir, "config_test.go")
			if err := os.WriteFile(generatedTest, []byte(testSource), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(t.Context(), "go", "test", "-count=1", "-timeout=1m", config, generatedTest).CombinedOutput()
			if err != nil {
				t.Fatalf("generated client: %v\n%s", err, out)
			}
			seen := h.onlyRequest(t)
			if seen.Model != h.upstreamModels[surface] {
				t.Fatalf("wrong model rewrite: %s", seen.Model)
			}
		})
	}
}
