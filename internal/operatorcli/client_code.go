package operatorcli

import (
	"encoding/json"
	"errors"
	"fmt"
	goformat "go/format"
	"strings"
)

// Code examples configure qualified clients without making requests or copying
// credentials. The consuming program reads its mounted key when it starts.
func clientCode(client, format, surface, origin, keyFile, model string) (string, error) {
	quote := func(value string) string { raw, _ := json.Marshal(value); return string(raw) }
	prefix := map[string]string{"openai": "/v1", "anthropic": "/anthropic", "gemini": "/gemini"}[surface]
	base, route, file := quote(origin+prefix), quote(model), quote(keyFile)
	framework := client != "openai" && client != "anthropic" && client != "gemini"
	if framework && format != "javascript" {
		return "", errors.New("qualified frameworks use --format javascript")
	}
	switch format {
	case "javascript":
		var imports, config string
		switch client {
		case "agents":
			if surface != "openai" {
				return "", errors.New("agents uses the OpenAI surface; choose a translated route for other upstreams")
			}
			imports = "import { Agent, OpenAIProvider, Runner } from '@openai/agents';\n"
			config = fmt.Sprintf("export const runner = new Runner({ modelProvider: new OpenAIProvider({ apiKey, baseURL: %s, useResponses: true }), tracingDisabled: true });\nexport const agent = new Agent({ name: 'Assistant', model: modelName, modelSettings: { store: false, maxTokens: 1024 } });\n", base)
		case "vercel":
			factory, pkg, endpoint := "createOpenAI", "openai", origin+prefix
			if surface == "anthropic" {
				factory, pkg, endpoint = "createAnthropic", "anthropic", origin+prefix+"/v1"
			}
			if surface == "gemini" {
				factory, pkg, endpoint = "createGoogleGenerativeAI", "google", origin+prefix+"/v1beta"
			}
			imports = fmt.Sprintf("import { %s } from '@ai-sdk/%s';\n", factory, pkg)
			method := ""
			if surface == "openai" {
				method = ".chat"
			}
			config = fmt.Sprintf("const provider = %s({ apiKey, baseURL: %s });\nexport const model = provider%s(modelName);\n", factory, quote(endpoint), method)
		case "langchain":
			class, pkg, options := "ChatOpenAI", "openai", "configuration: { baseURL: "+base+" }"
			if surface == "anthropic" {
				class, pkg, options = "ChatAnthropic", "anthropic", "anthropicApiUrl: "+base+", maxTokens: 1024"
			}
			if surface == "gemini" {
				class, pkg, options = "ChatGoogleGenerativeAI", "google-genai", "baseUrl: "+base
			}
			imports = fmt.Sprintf("import { %s } from '@langchain/%s';\n", class, pkg)
			config = fmt.Sprintf("export const client = new %s({ model: modelName, apiKey, maxRetries: 0, %s });\n", class, options)
		case "llamaindex":
			switch surface {
			case "openai":
				imports = "import { OpenAI } from '@llamaindex/openai';\n"
				config = "export const client = new OpenAI({ model: modelName, apiKey, baseURL: " + base + ", maxRetries: 0 });\n"
			case "anthropic":
				imports = "import { Anthropic, AnthropicSession } from '@llamaindex/anthropic';\n"
				config = "export const client = new Anthropic({ model: modelName, maxTokens: 1024, session: new AnthropicSession({ apiKey, baseURL: " + base + ", maxRetries: 0 }) });\n"
			case "gemini":
				imports = "import { Gemini } from '@llamaindex/google';\n"
				config = "export const client = new Gemini({ model: modelName, apiKey, maxRetries: 0, httpOptions: { baseUrl: " + base + " } });\n"
			}
		case "openai":
			imports = "import OpenAI from 'openai';\n"
			config = "export const client = new OpenAI({ apiKey, baseURL: " + base + ", maxRetries: 0 });\n"
		case "anthropic":
			imports = "import Anthropic from '@anthropic-ai/sdk';\n"
			config = "export const client = new Anthropic({ apiKey, baseURL: " + base + ", maxRetries: 0 });\n"
		case "gemini":
			imports = "import { GoogleGenAI } from '@google/genai';\n"
			config = "export const client = new GoogleGenAI({ apiKey, httpOptions: { baseUrl: " + base + ", apiVersion: 'v1beta' } });\n"
		}
		return "import { readFileSync } from 'node:fs';\n" + imports + "const apiKey = readFileSync(" + file + ", 'utf8').trim();\nexport const modelName = " + route + ";\n" + config, nil
	case "python":
		imports, config := "from openai import OpenAI\n", "client = OpenAI(api_key=api_key, base_url="+base+", max_retries=0)\n"
		if surface == "anthropic" {
			imports, config = "from anthropic import Anthropic\n", "client = Anthropic(api_key=api_key, base_url="+base+", max_retries=0)\n"
		}
		if surface == "gemini" {
			imports, config = "from google import genai\n", "client = genai.Client(api_key=api_key, http_options={'base_url': "+base+", 'api_version': 'v1beta'})\n"
		}
		return "from pathlib import Path\n" + imports + "api_key = Path(" + file + ").read_text(encoding='utf-8').strip()\nmodel_name = " + route + "\n" + config, nil
	case "go":
		imports, resultType, constructor := "\"github.com/openai/openai-go/v3\"\n\"github.com/openai/openai-go/v3/option\"", "openai.Client", "openai.NewClient(option.WithAPIKey(key), option.WithBaseURL("+base+"), option.WithMaxRetries(0)"
		zero := "openai.Client{}"
		if surface == "openai" {
			if strings.HasPrefix(origin, "http:") {
				constructor += ", option.WithUnsafeAllowHTTP()"
			}
			constructor += "), nil"
		}
		if surface == "anthropic" {
			imports = "\"github.com/anthropics/anthropic-sdk-go\"\nanthropicoption \"github.com/anthropics/anthropic-sdk-go/option\""
			resultType, zero, constructor = "anthropic.Client", "anthropic.Client{}", "anthropic.NewClient(anthropicoption.WithAPIKey(key), anthropicoption.WithBaseURL("+base+"), anthropicoption.WithMaxRetries(0)), nil"
		}
		if surface == "gemini" {
			imports = "\"google.golang.org/genai\""
			resultType, zero, constructor = "*genai.Client", "nil", "genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI, HTTPOptions: genai.HTTPOptions{BaseURL: "+base+", APIVersion: \"v1beta\"}})"
		}
		source := fmt.Sprintf("package olpclient\n\nimport (\n\"context\"\n\"errors\"\n\"os\"\n\"strings\"\n%s\n)\n\nconst Model = %s\n\nfunc New(ctx context.Context) (%s, error) {\nraw, err := os.ReadFile(%s)\nif err != nil { return %s, errors.New(\"cannot read mounted API key\") }\nkey := strings.TrimSpace(string(raw))\nclear(raw)\nreturn %s\n}\n", imports, route, resultType, file, zero, constructor)
		formatted, err := goformat.Source([]byte(source))
		return string(formatted), err
	default:
		return "", errors.New("--format must be shell, javascript, python or go")
	}
}
