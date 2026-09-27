package plugin

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type manifestOnly Manifest

func (m manifestOnly) Manifest() Manifest { return Manifest(m) }

type panicking struct{}

func (panicking) Manifest() Manifest { panic("declared nothing") }

func serveWith(t *testing.T, p Plugin, request string) abi.Response {
	t.Helper()
	registered = p
	t.Cleanup(func() { registered = nil })
	var response abi.Response
	if err := json.Unmarshal(serve([]byte(request)), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestServeAnswersTheManifestCall(t *testing.T) {
	declared := Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []Profile{{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat"}}}
	response := serveWith(t, manifestOnly(declared), `{"method":"manifest","params":null}`)
	var got Manifest
	if response.Error != nil || json.Unmarshal(response.Result, &got) != nil || !reflect.DeepEqual(got, declared) {
		t.Fatalf("manifest response %+v", response)
	}
}

func TestServeReportsFailuresAsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		plugin  Plugin
		request string
		code    string
	}{
		"unknown method":     {manifestOnly{}, `{"method":"sign"}`, abi.CodeUnknownMethod},
		"malformed":          {manifestOnly{}, `{"method":`, abi.CodeInvalidRequest},
		"nothing registered": {nil, `{"method":"manifest"}`, abi.CodeInternal},
		"panic":              {panicking{}, `{"method":"manifest"}`, abi.CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			if response := serveWith(t, tc.plugin, tc.request); response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("want %s, got %+v", tc.code, response)
			}
		})
	}
}

func TestLogRecordsFlattenAttributes(t *testing.T) {
	handler := hostHandler{}.WithAttrs([]slog.Attr{slog.String("plugin", "acme")}).WithGroup("grant").(hostHandler)
	r := slog.NewRecord(time.Time{}, slog.LevelWarn+1, "refresh failed", 0)
	r.AddAttrs(slog.Int("attempt", 2), slog.Group("upstream", slog.Int("status", 401)))
	got := handler.record(r)
	want := abi.LogRecord{Level: "warn", Message: "refresh failed", Attrs: map[string]string{"plugin": "acme", "grant.attempt": "2", "grant.upstream.status": "401"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record %+v, want %+v", got, want)
	}
}
