//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

type strictVideoUpstream struct {
	*httptest.Server
	mu                               sync.Mutex
	creates, gets, contents, deletes int
	activeGets, maxGets              int
	getDelay                         time.Duration
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
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/videos/upstream-video-") && !strings.Contains(r.URL.Path[len("/v1/videos/"):], "/"):
			f.mu.Lock()
			f.gets++
			f.activeGets++
			f.maxGets = max(f.maxGets, f.activeGets)
			delay := f.getDelay
			f.mu.Unlock()
			defer func() { f.mu.Lock(); f.activeGets--; f.mu.Unlock() }()
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q,"object":"video","model":%q,"status":"completed","progress":73.123456789,"created_at":1800000000,"completed_at":1800000060,"expires_at":4102444800,"seconds":"8","size":"1280x720","native":{"frame_ms":[-0,33.333333333],"large":9007199254740993}}`, strings.TrimPrefix(r.URL.Path, "/v1/videos/"), vendorModel)
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
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/videos/upstream-video-") && !strings.Contains(r.URL.Path[len("/v1/videos/"):], "/"):
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
	var retained int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose='media_job_source'").Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("video native content persisted: count=%d err=%v", retained, err)
	}
	var sourceID *string
	if err := h.Pool.QueryRow(t.Context(), "SELECT native_source_id::text FROM olp.media_jobs WHERE id=$1 AND strict_contract", localID).Scan(&sourceID); err != nil || sourceID != nil {
		t.Fatalf("job retained a native source: %v %v", sourceID, err)
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
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='media_job_source'", localID).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatalf("deleted secret retained: %d %v", secrets, err)
	}
	status, raw, _ = h.gatewayRaw("DELETE", "/v1/videos/"+localID, key, nil, nil)
	_, getAfter, _, deletes, _, _ := upstream.snapshot()
	if status != 404 || getAfter != gets || deletes != 1 {
		t.Fatalf("repeat delete dispatched or replayed success: %d %s", status, raw)
	}
}

func TestStrictVideoLifecycleSurvivesKeyRotationWithoutStoredContent(t *testing.T) {
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
	auth, err := secrets.DecodeKey(h.AuthHex)
	if err != nil {
		t.Fatal(err)
	}
	// A restarted process reads every secret with the rotated ring, the pinned
	// provider credential included.
	credentials := runtime.NewManager(h.Pool, h.Server.Installation, secrets.NewAuthKey(auth, h.Server.Installation), rotatedRing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := credentials.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := *h.Media
	restarted.Keys, restarted.Credentials = rotatedRing, credentials
	if target, _, code := restarted.JobTarget(t.Context(), &record); target == nil {
		t.Fatalf("rotated provider unavailable: %s", code)
	}
	var retained int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose='media_job_source'").Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("video native content persisted after rotation: count=%d err=%v", retained, err)
	}
}

func TestStrictVideoJobOnATransformedRouteIsListedAndDeletableButNotServed(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	upstream := newStrictVideoUpstream(t)
	slug, key := publishStrictVideo(t, h, owner, upstream)
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	form.WriteField("model", slug)
	form.WriteField("prompt", "retain the strict job")
	form.Close()
	status, raw, _ := h.gatewayRaw("POST", "/v1/videos", key, bytes.NewReader(upload.Bytes()), map[string]string{"Content-Type": form.FormDataContentType()})
	if status != http.StatusCreated {
		t.Fatalf("strict video create=%d %s", status, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	route := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	providerID := route["latest_revision"].(map[string]any)["targets"].([]any)[0].(map[string]any)["provider_id"]
	draft := transformed(fidelityDraft(slug, providerID))
	draft["operations"] = []string{"video_create", "video_get", "video_content", "video_delete"}
	draft = h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
	_, getsBefore, contentsBefore, deletesBefore, _, _ := upstream.snapshot()
	for _, path := range []string{"/v1/videos/" + created.ID, "/v1/videos/" + created.ID + "/content"} {
		status, raw, _ = h.gatewayRaw("GET", path, key, nil, nil)
		if status != http.StatusConflict || !strings.Contains(string(raw), "now transformed") {
			t.Fatalf("GET %s served a strict job on a transformed route: %d %s", path, status, raw)
		}
	}
	// The list keeps the queued job and shows its stored state instead of
	// polling the provider through a contract the route no longer promises.
	status, raw, _ = h.gatewayRaw("GET", "/v1/videos", key, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), created.ID) || !strings.Contains(string(raw), `"status":"queued"`) {
		t.Fatalf("video list after the route became transformed: %d %s", status, raw)
	}
	if _, gets, contents, deletes, _, _ := upstream.snapshot(); gets != getsBefore || contents != contentsBefore || deletes != deletesBefore {
		t.Fatal("refused or listed strict video job reached the provider")
	}
	// Its owner can still delete it, so the provider-held video is removed.
	status, raw, _ = h.gatewayRaw("DELETE", "/v1/videos/"+created.ID, key, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), created.ID) {
		t.Fatalf("delete of a strict job on a transformed route: %d %s", status, raw)
	}
	if _, _, _, deletes, _, _ := upstream.snapshot(); deletes != deletesBefore+1 {
		t.Fatal("strict video delete did not reach the provider")
	}
}

// revokedCredentials is a credential source whose authority revoked every
// credential version.
type revokedCredentials struct{ runtime.Credentials }

func (revokedCredentials) Eligibility(string) runtime.Eligibility { return runtime.Revoked }
func TestVideoJobOutsideTheKeysRoutesIsMissing(t *testing.T) {
	h := newAccessHarness(t)
	upstream := newStrictVideoUpstream(t)
	slug, key := publishStrictVideo(t, h, h.owner(), upstream)
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	form.WriteField("model", slug)
	form.WriteField("prompt", "a job the key later loses")
	form.Close()
	status, raw, _ := h.gatewayRaw("POST", "/v1/videos", key, bytes.NewReader(upload.Bytes()), map[string]string{"Content-Type": form.FormDataContentType()})
	if status != 201 {
		t.Fatalf("video create=%d %s", status, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	authority, err := h.authority(key)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `UPDATE olp.api_keys SET policy=jsonb_set(policy,'{allowed_routes}','["elsewhere"]') WHERE id=$1`, authority.ID); err != nil {
		t.Fatal(err)
	}
	// Advance the key authority as a key change through the API does.
	if _, err = tx.Exec(t.Context(), "UPDATE olp.installation SET authority_id=$1,authority_sequence=authority_sequence+1 WHERE singleton", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.refresh()
	// The owning key may no longer use the job's route, so the job answers
	// like any other retained resource outside the key's reach.
	status, raw, _ = h.gatewayRaw("GET", "/v1/videos/"+created.ID, key, nil, nil)
	if status != 404 || !bytes.Contains(raw, []byte("video_not_found")) {
		t.Fatalf("a video job outside the key's routes: %d %s", status, raw)
	}
}

func TestVideoListDoesNotMultiplyUpstreamConcurrency(t *testing.T) {
	h := newAccessHarness(t)
	upstream := newStrictVideoUpstream(t)
	slug, key := publishStrictVideo(t, h, h.owner(), upstream)
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	form.WriteField("model", slug)
	form.WriteField("prompt", "private video prompt")
	form.Close()
	status, raw, _ := h.gatewayRaw("POST", "/v1/videos", key, bytes.NewReader(upload.Bytes()), map[string]string{"Content-Type": form.FormDataContentType()})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || status != 201 {
		t.Fatalf("create: %d %s %v", status, raw, err)
	}
	for i := 2; i <= 5; i++ {
		_, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.media_jobs SELECT (jsonb_populate_record(NULL::olp.media_jobs,to_jsonb(j)||jsonb_build_object('id',$1::text,'upstream_job_id',$2::text))).* FROM olp.media_jobs j WHERE id=$3`, uuid.NewString(), fmt.Sprintf("upstream-video-%d", i), created.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	upstream.mu.Lock()
	upstream.getDelay = 20 * time.Millisecond
	upstream.mu.Unlock()
	status, raw, _ = h.gatewayRaw("GET", "/v1/videos?limit=100", key, nil, nil)
	if status != 200 {
		t.Fatalf("list: %d %s", status, raw)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.gets != 5 || upstream.maxGets != 1 {
		t.Fatalf("list polls=%d max simultaneous=%d", upstream.gets, upstream.maxGets)
	}
}
