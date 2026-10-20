package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/resources"
)

type allowGeminiEndpoint struct{ reject bool }

func (policy allowGeminiEndpoint) ValidateEndpoint(value string) (*url.URL, error) {
	if policy.reject {
		return nil, errors.New("outside egress")
	}
	return url.Parse(value)
}

func TestGeminiUploadStartValidation(t *testing.T) {
	if length, err := validateGeminiStartHeaders("resumable", "start", "12", 16); err != nil || length != 12 {
		t.Fatalf("valid start headers length=%d err=%v", length, err)
	}
	for _, header := range [][3]string{
		{"multipart", "start", "12"},
		{"resumable", "upload", "12"},
		{"resumable", "start", "0"},
		{"resumable", "start", "17"},
		{"resumable", "start", "nope"},
	} {
		if _, err := validateGeminiStartHeaders(header[0], header[1], header[2], 16); err == nil {
			t.Fatalf("accepted invalid start headers %v", header)
		}
	}
	if name, present, err := parseGeminiUploadStartBody(nil); err != nil || present || name != "" {
		t.Fatalf("empty start body name=%q present=%v err=%v", name, present, err)
	}
	for _, body := range []string{
		`{"displayName":"name"}`,
		`{"file":{"displayName":3}}`,
		`{"file":{"displayName":"x","display_name":"y"}}`,
		`{"file":{"other":"name"}}`,
	} {
		if _, _, err := parseGeminiUploadStartBody([]byte(body)); err == nil {
			t.Fatalf("accepted invalid start body %s", body)
		}
	}
	if name, present, err := parseGeminiUploadStartBody([]byte(`{"file":{"display_name":"sample"}}`)); err != nil || !present || name != "sample" {
		t.Fatalf("display_name body name=%q present=%v err=%v", name, present, err)
	}
}

func TestGeminiUploadURLValidation(t *testing.T) {
	cfgEndpoint := "https://generativelanguage.googleapis.com/v1beta"
	cfg := connectors.Config{Kind: "gemini", Endpoint: cfgEndpoint}
	endpoint, err := geminiUploadOrigin(cfg, "model-a", allowGeminiEndpoint{})
	if err != nil || endpoint != "https://generativelanguage.googleapis.com/upload/v1beta/files" {
		t.Fatalf("upload origin=%q err=%v", endpoint, err)
	}
	if err := validateGeminiUploadURL("https://generativelanguage.googleapis.com/upload/session?token=x", cfgEndpoint, allowGeminiEndpoint{}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"http://generativelanguage.googleapis.com/upload/session",
		"https://evil.example/upload/session",
		"https://user@generativelanguage.googleapis.com/upload/session",
		"https://generativelanguage.googleapis.com/upload/session#fragment",
	} {
		if err := validateGeminiUploadURL(value, cfgEndpoint, allowGeminiEndpoint{}); err == nil {
			t.Fatalf("accepted unsafe upload URL %s", value)
		}
	}
	if err := validateGeminiUploadURL("https://generativelanguage.googleapis.com/upload/session", cfgEndpoint, allowGeminiEndpoint{reject: true}); err == nil {
		t.Fatal("accepted upload URL rejected by egress policy")
	}
}

func TestGeminiUploadCommandOffsetAndProjection(t *testing.T) {
	for value, want := range map[string]geminiCommand{
		"upload":           {},
		"upload, finalize": {finalize: true},
		"query":            {query: true},
	} {
		got, err := parseGeminiUploadCommand(value)
		if err != nil || got != want {
			t.Fatalf("command %q = %+v, %v", value, got, err)
		}
	}
	if _, err := parseGeminiUploadCommand("start"); err == nil {
		t.Fatal("accepted start command on a session")
	}
	if err := validateGeminiChunk(10, 4, 4, 6, true); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		expected, stored, offset, length int64
		finalize                         bool
	}{
		{10, 4, 3, 6, true},
		{10, 4, 4, 7, false},
		{10, 4, 4, 5, true},
	} {
		if err := validateGeminiChunk(test.expected, test.stored, test.offset, test.length, test.finalize); err == nil {
			t.Fatalf("accepted invalid chunk %+v", test)
		}
	}
	upstream := []byte(`{"name":"files/upstream","uri":"https://provider/files/upstream","downloadUri":"https://provider/download/upstream","mimeType":"application/pdf","sizeBytes":"5","sha256Hash":"hash","state":"ACTIVE"}`)
	projected, err := projectGeminiFile(upstream, "gemini_file_local", "https://gateway.example")
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if json.Unmarshal(projected, &object) != nil || object["name"] != "files/gemini_file_local" ||
		object["uri"] != "https://gateway.example/gemini/v1beta/files/gemini_file_local" ||
		object["downloadUri"] != "https://gateway.example/gemini/download/v1beta/files/gemini_file_local:download?alt=media" ||
		object["mimeType"] != "application/pdf" || object["sha256Hash"] != "hash" {
		t.Fatalf("Gemini file projection=%s", projected)
	}
}

func TestGeminiFileListCursorAndRoutePrecedence(t *testing.T) {
	id := resources.LocalID(resources.KindGeminiFile, uuid.New())
	if pageSize, token, err := geminiPageCursor(url.Values{"pageToken": {id}, "pageSize": {"100"}}); err != nil ||
		pageSize != 100 || token != id {
		t.Fatalf("page cursor=%d/%q err=%v", pageSize, token, err)
	}
	for _, query := range []url.Values{{"pageSize": {"101"}}, {"pageSize": {"0"}}, {"pageToken": {"invalid"}}} {
		if _, _, err := geminiPageCursor(query); err == nil {
			t.Fatalf("accepted invalid Gemini cursor %v", query)
		}
	}
	server := &Server{Resources: resources.New(nil), Media: &MediaDeps{}}
	mux := http.NewServeMux()
	server.registerNative(mux)
	server.registerState(mux)
	for _, test := range []struct{ method, path, want string }{
		{http.MethodPost, "/gemini/upload/v1beta/files", "POST /gemini/upload/v1beta/files"},
		{http.MethodPost, "/gemini/upload/gemini_upload_id", "POST /gemini/upload/{session}"},
		{http.MethodGet, "/gemini/v1beta/files/gemini_file_id", "GET /gemini/v1beta/files/{name}"},
		{http.MethodGet, "/gemini/download/v1beta/files/gemini_file_id:download", "GET /gemini/download/v1beta/files/{action}"},
		{http.MethodPost, "/gemini/v1beta/models/gemini-2.5:generateContent", "POST /gemini/v1beta/models/{action}"},
		{http.MethodPost, "/gemini/v1beta/models/gemini-2.5:batchGenerateContent", "POST /gemini/v1beta/models/{action}"},
		{http.MethodGet, "/gemini/v1beta/batches", "GET /gemini/v1beta/batches"},
		{http.MethodGet, "/gemini/v1beta/batches/gemini_batch_id", "GET /gemini/v1beta/batches/{name}"},
		{http.MethodPost, "/gemini/v1beta/batches/gemini_batch_id:cancel", "POST /gemini/v1beta/batches/{action}"},
		{http.MethodDelete, "/gemini/v1beta/batches/gemini_batch_id", "DELETE /gemini/v1beta/batches/{name}"},
	} {
		_, pattern := mux.Handler(&http.Request{Method: test.method, URL: &url.URL{Path: test.path}})
		if pattern != test.want {
			t.Fatalf("%s %s matched %q want %q", test.method, test.path, pattern, test.want)
		}
	}
}
