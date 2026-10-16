package protocols

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func structuredToolRequest(t *testing.T, family wire.Format, surface, value string) (*wire.Request, string) {
	t.Helper()
	block := `{"toolUse":{"toolUseId":"call","name":"lookup","input":` + value + `}}`
	path := "/messages/0/content/0/toolUse/input"
	if surface == "result" {
		block = `{"toolResult":{"toolUseId":"call","content":[{"json":` + value + `}]}}`
		path = "/messages/0/content/0/toolResult/content/0/json"
	}
	body := `{"messages":[{"role":"user","content":[` + block + `,{"text":"after tool data"}]}]}`
	if family == "bedrock_count" {
		body = `{"input":{"converse":` + body + `}}`
		path = "/input/converse" + path
	}
	request, err := wire.FromFields(family, "route", false, decodeFields(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return request, path
}
