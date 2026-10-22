package coderesearch

import (
	"encoding/json"
	"testing"
)

func TestResearchPingResponsesAreNarrowAndPreserveIDs(t *testing.T) {
	for _, id := range []string{`1`, `9007199254740993`, `"server-ping"`, `"escaped\u002did"`} {
		raw := []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
		reply, err := PingResponse(raw)
		if err != nil || !json.Valid(reply) {
			t.Fatalf("ping response: %s %v", reply, err)
		}
		var result map[string]json.RawMessage
		if json.Unmarshal(reply, &result) != nil || !sameID(result["id"], json.RawMessage(id)) || string(result["result"]) != "{}" || len(result) != 3 {
			t.Fatalf("ping response changed ID or payload: %s", reply)
		}
		if _, _, err := Request(reply, "search"); err == nil {
			t.Fatal("client can forge a server callback response")
		}
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","id":{},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","result":{}}`,
	} {
		if _, err := PingResponse([]byte(raw)); err == nil {
			t.Fatalf("malformed ping accepted: %s", raw)
		}
	}
	if reply, err := PingResponse([]byte(`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage","params":{}}`)); err != nil || reply != nil {
		t.Fatal("enabled an unadvertised server callback")
	}
}
