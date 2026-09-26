//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/secrets"
)

type strictVideoUpstream struct {
	*httptest.Server
	mu                               sync.Mutex
	creates, gets, contents, deletes int
	fields                           []string
	values                           [][]byte
}

func newStrictVideoUpstream(t *testing.T) *strictVideoUpstream {
	t.Helper()
	f := &strictVideoUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos":
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				http.Error(w, "invalid multipart", 400)
				return
			}
			var names []string
			var values [][]byte
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				body, err := io.ReadAll(io.LimitReader(part, 1<<20))
				if err != nil {
					t.Error(err)
					return
				}
				names, values = append(names, part.FormName()), append(values, body)
				if part.FormName() == "input_reference" && (part.FileName() != "first-frame.png" || part.Header.Get("Content-Type") != "image/png") {
					t.Error("video source filename/type changed")
				}
			}
			f.mu.Lock()
			f.creates++
			f.fields, f.values = names, values
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"upstream-video-1","object":"video","model":%q,"status":"queued","progress":0,"created_at":1800000000,"seconds":"8","size":"1280x720","native":{"large":9007199254740993}}`, vendorModel)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/upstream-video-1":
			f.mu.Lock()
			f.gets++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"upstream-video-1","object":"video","model":%q,"status":"completed","progress":73.123456789,"created_at":1800000000,"completed_at":1800000060,"expires_at":4102444800,"seconds":"8","size":"1280x720","native":{"frame_ms":[-0,33.333333333],"large":9007199254740993}}`, vendorModel)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/upstream-video-1/content":
			f.mu.Lock()
			f.contents++
			f.mu.Unlock()
			if r.URL.Query().Get("variant") == "thumbnail" {
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write([]byte{0xff, 0xd8, 0, 1, 0xff, 0xd9})
			} else {
				w.Header().Set("Content-Type", `video/mp4; codecs="avc1.42E01E"`)
				w.Write([]byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'})
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/videos/upstream-video-1":
			f.mu.Lock()
			f.deletes++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"upstream-video-1","object":"video.deleted","deleted":true,"native":{"large":9007199254740993}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *strictVideoUpstream) snapshot() (int, int, int, int, []string, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.gets, f.contents, f.deletes, append([]string(nil), f.fields...), append([][]byte(nil), f.values...)
}

func publishStrictVideo(t *testing.T, h *accessHarness, owner *browser, upstream *strictVideoUpstream) (string, string) {
	t.Helper()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          "Strict video " + uuid.NewString(),
		"configuration": map[string]any{"kind": "openai_compatible", "profile_id": "compatible-chat", "profile_revision": "1", "auth_mode": "api_key", "endpoint": upstream.URL + "/v1"},
		"model":         vendorModel, "credential": vendorSecret,
	}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + provider["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(provider), 200)
	if probe["succeeded"] != true {
		t.Fatal("video fixture model probe failed", probe)
	}
	certifyStrictMediaFixture(t, h, provider["id"].(string), "video_create", "async",
		[2]string{"video_list", "unary"}, [2]string{"video_get", "unary"}, [2]string{"video_content", "unary"}, [2]string{"video_delete", "unary"})
	provider = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	// The provider list is project-wide while OLP's existing list is owned by
	// one API key. A strict route cannot mislabel that local inventory as the
	// provider's native collection, even when the tuple is certified.
	unsupported := fidelityDraft("strict-video-list-"+uuid.NewString(), provider["id"])
	unsupported["operations"] = []string{"video_create", "video_list", "video_get", "video_content", "video_delete"}
	unsupported["fidelity"] = map[string]any{"mode": "strict"}
	draftList := h.want(owner, "POST", "/api/v1/route-drafts", unsupported, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draftList["id"].(string)+"/activate", nil, withMatch(draftList, idem(uuid.NewString())), 422)
	slug := "strict-video-" + uuid.NewString()
	draftInput := fidelityDraft(slug, provider["id"])
	draftInput["operations"] = []string{"video_create", "video_get", "video_content", "video_delete"}
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Strict video", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func TestStrictVideoPublicOriginalAssetsAndDurableIdentity(t *testing.T) {
	h := newAccessHarness(t)
	upstream := newStrictVideoUpstream(t)
	slug, key := publishStrictVideo(t, h, h.owner(), upstream)
	status, raw, _ := h.gatewayRaw("GET", "/v1/videos?limit=1&order=desc", key, nil, nil)
	if status != 200 || !bytes.Contains(raw, []byte(`"object":"list"`)) {
		t.Fatalf("owner-scoped video inventory unavailable: %d %s", status, raw)
	}
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	form.WriteField("prompt", "one frame, eight seconds")
	form.WriteField("seconds", "8")
	header := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="input_reference"; filename="first-frame.png"`}, "Content-Type": {"image/png"}}
	part, err := form.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	pixel := []byte{0x89, 0x50, 0x4e, 0x47, 0, 1, 0xff}
	part.Write(pixel)
	form.WriteField("model", slug)
	form.WriteField("size", "1280x720")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	createsBefore, _, _, _, _, _ := upstream.snapshot()
	status, raw, _ = h.gatewayRaw("POST", "/v1/videos", key, bytes.NewReader(upload.Bytes()), map[string]string{"Content-Type": form.FormDataContentType(), "Idempotency-Key": "unqualified-native-promise"})
	createsAfter, _, _, _, _, _ := upstream.snapshot()
	if status != 400 || createsAfter != createsBefore {
		t.Fatalf("unqualified create header dispatched: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw("POST", "/v1/videos", key, bytes.NewReader(upload.Bytes()), map[string]string{"Content-Type": form.FormDataContentType()})
	if status != http.StatusCreated {
		t.Fatalf("strict video create=%d %s", status, raw)
	}
	var created map[string]json.RawMessage
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	var localID string
	if err := json.Unmarshal(created["id"], &localID); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(localID); err != nil {
		t.Fatalf("local job identity=%q", localID)
	}
	wantCreate := fmt.Sprintf(`{"id":%q,"object":"video","model":%q,"status":"queued","progress":0,"created_at":1800000000,"seconds":"8","size":"1280x720","native":{"large":9007199254740993}}`, localID, slug)
	if string(raw) != wantCreate {
		t.Fatalf("native create metadata changed: %s", raw)
	}
	creates, _, _, _, names, values := upstream.snapshot()
	if creates != 1 || strings.Join(names, ",") != "prompt,seconds,input_reference,model,size" || len(values) != 5 || string(values[0]) != "one frame, eight seconds" || string(values[1]) != "8" || !bytes.Equal(values[2], pixel) || string(values[3]) != vendorModel || string(values[4]) != "1280x720" {
		t.Fatalf("native multipart source changed: names=%v values=%q", names, values)
	}
	status, raw, _ = h.gatewayRaw("GET", "/v1/videos?limit=1&order=desc", key, nil, nil)
	if status != 200 || !bytes.Contains(raw, []byte(localID)) {
		t.Fatalf("owner-scoped video inventory lost job: %d %s", status, raw)
	}
	var ownerID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT api_key_id::text FROM olp_go.media_jobs WHERE id=$1 AND strict_contract AND native_source_id=$1", localID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := h.Pool.QueryRow(t.Context(), "SELECT ciphertext FROM olp_go.secrets WHERE id=$1 AND purpose='media_job_source'", localID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("one frame")) || bytes.Contains(ciphertext, []byte("9007199254740993")) {
		t.Fatal("native metadata stored as plaintext")
	}
	record, err := media.Job(t.Context(), h.Pool, localID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := *h.Media
	source, err := recovered.ReadNativeVideoSource(t.Context(), record, ownerID)
	if err != nil || !bytes.Contains(source, []byte("9007199254740993")) {
		t.Fatalf("restart source unavailable: %v", err)
	}
	orphanID := uuid.NewString()
	_, err = recovered.AttachWithRetry(t.Context(), orphanID, "orphan-video", media.JobUpdate{State: media.StateQueued, LastPolledAt: time.Now()}, source)
	if err == nil {
		t.Fatal("unreserved video source activated")
	}
	var orphanSecrets int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.secrets WHERE id=$1 AND purpose='media_job_source'", orphanID).Scan(&orphanSecrets); err != nil || orphanSecrets != 0 {
		t.Fatalf("failed attachment committed a source secret: %d %v", orphanSecrets, err)
	}
	if _, err = recovered.ReadNativeVideoSource(t.Context(), record, uuid.NewString()); err == nil {
		t.Fatal("wrong owner read native source")
	}
	past := time.Now().Add(-time.Second)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.media_jobs SET expires_at=$2 WHERE id=$1", localID, past); err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.ReadNativeVideoSource(t.Context(), record, ownerID); err == nil {
		t.Fatal("expired job read native source")
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.media_jobs SET expires_at=$2 WHERE id=$1", localID, record.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	oldRevoked := recovered.Revoked
	recovered.Revoked = func(string) bool { return true }
	if _, err = recovered.ReadNativeVideoSource(t.Context(), record, ownerID); err == nil {
		t.Fatal("revoked provider read native source")
	}
	recovered.Revoked = oldRevoked
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.secrets SET ciphertext=$2 WHERE id=$1 AND purpose='media_job_source'", localID, []byte{0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.ReadNativeVideoSource(t.Context(), record, ownerID); err == nil {
		t.Fatal("tampered video source authenticated")
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.secrets SET ciphertext=$2 WHERE id=$1 AND purpose='media_job_source'", localID, ciphertext); err != nil {
		t.Fatal(err)
	}

	status, raw, _ = h.gatewayRaw("GET", "/v1/videos/"+localID, key, nil, nil)
	wantGet := fmt.Sprintf(`{"id":%q,"object":"video","model":%q,"status":"completed","progress":73.123456789,"created_at":1800000000,"completed_at":1800000060,"expires_at":4102444800,"seconds":"8","size":"1280x720","native":{"frame_ms":[-0,33.333333333],"large":9007199254740993}}`, localID, slug)
	if status != 200 || string(raw) != wantGet {
		t.Fatalf("native get=%d %s", status, raw)
	}
	for _, content := range []struct {
		path, mediaType string
		body            []byte
	}{{"", `video/mp4; codecs="avc1.42E01E"`, []byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'}}, {"?variant=thumbnail", "image/jpeg", []byte{0xff, 0xd8, 0, 1, 0xff, 0xd9}}} {
		status, raw, header := h.gatewayRaw("GET", "/v1/videos/"+localID+"/content"+content.path, key, nil, nil)
		if status != 200 || header.Get("Content-Type") != content.mediaType || !bytes.Equal(raw, content.body) {
			t.Fatalf("native content=%d %s %x", status, header.Get("Content-Type"), raw)
		}
	}
	_, gets, contents, _, _, _ := upstream.snapshot()
	status, raw, _ = h.gatewayRaw("GET", "/v1/videos/"+localID+"/content?variant=thumbnail&variant=video", key, nil, nil)
	_, _, afterContents, _, _, _ := upstream.snapshot()
	if status != 400 || contents != afterContents {
		t.Fatalf("ambiguous variant dispatched: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw("DELETE", "/v1/videos/"+localID, key, nil, nil)
	wantDelete := fmt.Sprintf(`{"id":%q,"object":"video.deleted","deleted":true,"native":{"large":9007199254740993}}`, localID)
	if status != 200 || string(raw) != wantDelete {
		t.Fatalf("native delete=%d %s", status, raw)
	}
	var secrets int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.secrets WHERE id=$1 AND purpose='media_job_source'", localID).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatalf("deleted secret retained: %d %v", secrets, err)
	}
	status, raw, _ = h.gatewayRaw("DELETE", "/v1/videos/"+localID, key, nil, nil)
	_, getAfter, _, deletes, _, _ := upstream.snapshot()
	if status != 404 || getAfter != gets || deletes != 1 {
		t.Fatalf("repeat delete dispatched or replayed success: %d %s", status, raw)
	}
}

func TestStrictVideoNativeSourceSurvivesKeyRotation(t *testing.T) {
	h := newAccessHarness(t)
	upstream := newStrictVideoUpstream(t)
	slug, key := publishStrictVideo(t, h, h.owner(), upstream)
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	form.WriteField("model", slug)
	form.WriteField("prompt", "retain the original job")
	form.Close()
	status, raw, _ := h.gatewayRaw("POST", "/v1/videos", key, bytes.NewReader(upload.Bytes()), map[string]string{"Content-Type": form.FormDataContentType()})
	if status != 201 {
		t.Fatalf("video create before rotation=%d %s", status, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	record, err := media.Job(t.Context(), h.Pool, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotatedRing, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat("ef", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	count, err := rotatedRing.Rotate(t.Context(), h.Pool, h.Server.Installation)
	if err != nil || count == 0 {
		t.Fatalf("master key rotation did not include video source: %d %v", count, err)
	}
	restarted := *h.Media
	restarted.Keys = rotatedRing
	source, err := restarted.ReadNativeVideoSource(t.Context(), record, record.APIKeyID)
	if err != nil || !bytes.Contains(source, []byte("9007199254740993")) {
		t.Fatalf("rotated source unavailable: %v", err)
	}
	if _, err := h.Media.ReadNativeVideoSource(t.Context(), record, record.APIKeyID); err == nil {
		t.Fatal("retired master key read rotated source")
	}
}
