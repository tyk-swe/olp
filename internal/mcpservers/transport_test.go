package mcpservers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeToolResultAcceptsTextAndMedia(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("bytes"))
	raw := json.RawMessage(`{"content":[{"type":"text","text":"hello"},{"type":"image","data":"` + data + `","mimeType":"image/png"}],"structuredContent":{"ok":true}}`)
	result, err := decodeToolResult(raw)
	if err != nil {
		t.Fatalf("decodeToolResult: %v", err)
	}
	if len(result.Content) != 2 || result.Content[0].Text != "hello" || result.Content[1].Data != data || result.Content[1].MimeType != "image/png" {
		t.Fatalf("result = %+v", result)
	}
	if result.IsError {
		t.Fatal("result marked as error")
	}
}

func TestDecodeToolResultRejections(t *testing.T) {
	cases := []string{
		`{}`,
		`{"content":null}`,
		`{"content":[{"type":"text"}]}`,
		`{"content":[{"type":"text","text":"x","data":"eA=="}]}`,
		`{"content":[{"type":"image","data":"not-base64!","mimeType":"image/png"}]}`,
		`{"content":[{"type":"image","data":"eA=="}]}`,
		`{"content":[{"type":"resource","text":"x"}]}`,
		`{"content":[],"structuredContent":[1,2]}`,
		`{"content":[],"unknown":true}`,
	}
	for _, raw := range cases {
		if _, err := decodeToolResult(json.RawMessage(raw)); !errors.Is(err, ErrTransport) {
			t.Errorf("decodeToolResult(%s) err = %v, want ErrTransport", raw, err)
		}
	}
}

func TestDecodeResultRequiresMatchingReply(t *testing.T) {
	var out struct {
		Value string `json:"value"`
	}
	if err := decodeResult([]byte(`{"jsonrpc":"2.0","id":7,"result":{"value":"ok"}}`), 7, &out); err != nil || out.Value != "ok" {
		t.Fatalf("decodeResult = %v, %+v", err, out)
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":8,"result":{"value":"ok"}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-1,"message":"no"}}`,
		`{"jsonrpc":"2.0","id":7,"result":null}`,
		`{"jsonrpc":"1.0","id":7,"result":{"value":"ok"}}`,
		`{"id":7,"result":{"value":"ok"}}`,
	} {
		if err := decodeResult([]byte(raw), 7, &out); !errors.Is(err, ErrTransport) {
			t.Errorf("decodeResult(%s) err = %v, want ErrTransport", raw, err)
		}
	}
}

func TestObserveNotificationOnlyMarksToolListChanges(t *testing.T) {
	transport := &Transport{}
	transport.observeNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","id":3}`))
	if transport.ListChanged() {
		t.Fatal("request-shaped message marked the list changed")
	}
	transport.observeNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/progress"}`))
	if transport.ListChanged() {
		t.Fatal("unrelated notification marked the list changed")
	}
	transport.observeNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	if !transport.ListChanged() {
		t.Fatal("list_changed notification was ignored")
	}
}

func TestToolContentMarshalOmitsForeignFields(t *testing.T) {
	text, err := json.Marshal(ToolContent{Type: "text", Text: "hi", Data: "eA==", MimeType: "image/png"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(text) != `{"type":"text","text":"hi"}` {
		t.Fatalf("text content = %s", text)
	}
	media, err := json.Marshal(ToolContent{Type: "audio", Text: "hi", Data: "eA==", MimeType: "audio/wav"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(media) != `{"type":"audio","data":"eA==","mimeType":"audio/wav"}` {
		t.Fatalf("media content = %s", media)
	}
}
