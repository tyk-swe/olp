//go:build integration

package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/providers"
)

type videoUpstream struct {
	*httptest.Server
	status       atomic.Value
	progress     atomic.Value
	createCalls  atomic.Int64
	getCalls     atomic.Int64
	deleteCalls  atomic.Int64
	contentCalls atomic.Int64
	contentBytes string
}

func newVideoUpstream(t *testing.T) *videoUpstream {
	t.Helper()
	u := &videoUpstream{contentBytes: "video-bytes-marker-5"}
	u.status.Store("queued")
	var progress float32
	u.progress.Store(&progress)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, vendorModel)
	})
	mux.HandleFunc("POST /v1/videos", func(w http.ResponseWriter, r *http.Request) {
		n := u.createCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"vid-up-%d","object":"video","model":%q,"status":"queued","created_at":1}`, n, vendorModel)
	})
	mux.HandleFunc("GET /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		u.getCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		status := u.status.Load().(string)
		progress := u.progress.Load().(*float32)
		fmt.Fprintf(w, `{"id":%q,"object":"video","model":%q,"status":%q,"progress":%v,"created_at":1}`,
			r.PathValue("id"), vendorModel, status, *progress)
	})
	mux.HandleFunc("GET /v1/videos/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		u.contentCalls.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		io.WriteString(w, u.contentBytes+"-"+r.URL.Query().Get("variant"))
	})
	mux.HandleFunc("DELETE /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		u.deleteCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"video.deleted","deleted":true}`, r.PathValue("id"))
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Server.Close)
	return u
}

func certifyVideoSeeded(t *testing.T, h *accessHarness, providerID string) {
	t.Helper()
	var cfgRaw []byte
	if err := h.Pool.QueryRow(t.Context(), "SELECT configuration FROM olp.providers WHERE id=$1", providerID).Scan(&cfgRaw); err != nil {
		t.Fatal(err)
	}
	var cfg providers.Configuration
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatal(err)
	}
	transportInput, _ := json.Marshal([]any{cfg.Kind, cfg.AuthMode, cfg.Endpoint, cfg.CloudRegion, cfg.CloudProject, cfg.Deployment, cfg.APIVersion, cfg.Options.CredentialHeaders, cfg.Options.ParameterDefaults, cfg.Options.Models, cfg.Options.VendorID})
	transportSum := sha256.Sum256(transportInput)
	transportFP := hex.EncodeToString(transportSum[:])[:32]
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT credential_id::text FROM olp.provider_slots WHERE provider_id=$1 AND is_default", providerID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	credentialFP := transportFP + ":" + credentialID
	tuples := []string{}
	for _, c := range [][3]string{
		{"video_create", "openai", "async"},
		{"video_content", "openai", "unary"},
		{"video_delete", "openai", "unary"},
		{"video_get", "openai", "unary"},
	} {
		enc, _ := json.Marshal([]string{vendorModel, c[0], c[1], c[2]})
		tuples = append(tuples, string(enc))
	}
	sort.Strings(tuples)
	tuplesJSON, _ := json.Marshal(tuples)
	tupleSum := sha256.Sum256(tuplesJSON)
	validatedFP := credentialFP + ":" + hex.EncodeToString(tupleSum[:])
	now := time.Now().UTC()
	caps := []map[string]any{}
	for _, c := range [][3]string{
		{"video_create", "openai", "async"},
		{"video_get", "openai", "unary"},
		{"video_content", "openai", "unary"},
		{"video_delete", "openai", "unary"},
	} {
		caps = append(caps, map[string]any{
			"operation": c[0], "surface": c[1], "mode": c[2],
			"source": "certified", "certified_at": now.Format(time.RFC3339Nano),
			"credential_fingerprint": credentialFP,
		})
	}
	capsJSON, _ := json.Marshal(caps)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.provider_models SET capabilities=$1 WHERE provider_id=$2 AND upstream_model=$3", capsJSON, providerID, vendorModel); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.provider_slots SET validated_at=$1, validated_fingerprint=$2 WHERE provider_id=$3 AND is_default", now, validatedFP, providerID); err != nil {
		t.Fatal(err)
	}
}

func provisionVideo(t *testing.T, h *accessHarness, b *browser, endpoint string, projectID any) (map[string]any, string) {
	t.Helper()
	create := map[string]any{
		"name":          "Video provider",
		"configuration": map[string]any{"kind": "openai", "auth_mode": "api_key", "endpoint": endpoint},
		"credential":    vendorSecret,
		"model":         vendorModel,
	}
	if projectID != nil {
		create["project_id"] = projectID
	}
	detail := h.want(b, "POST", "/api/v1/providers", create, idem("video-provider"), 201)
	path := "/api/v1/providers/" + detail["id"].(string)
	probe := h.want(b, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("probe must succeed: %v", probe)
	}
	certifyVideoSeeded(t, h, detail["id"].(string))
	detail = h.want(b, "GET", path, nil, nil, 200)
	h.want(b, "POST", path+"/activate", nil, withMatch(detail, idem("video-activate")), 200)
	slug := "video-" + uuid.NewString()[:8]
	draft := map[string]any{
		"slug": slug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"operations": []string{"video_create", "video_get", "video_content", "video_delete"},
		"targets":    []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}
	if projectID != nil {
		draft["project_id"] = projectID
	}
	created := h.want(b, "POST", "/api/v1/route-drafts", draft, idem("video-draft-"+slug), 201)
	h.want(b, "POST", "/api/v1/route-drafts/"+created["id"].(string)+"/activate", nil, withMatch(created, idem("video-draft-activate")), 200)
	return detail, slug
}

func (h *accessHarness) videoCreate(slug, secret string) (int, map[string]any) {
	h.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", slug); err != nil {
		h.t.Fatal(err)
	}
	if err := form.WriteField("prompt", "a two second clip of rain"); err != nil {
		h.t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		h.t.Fatal(err)
	}
	r, err := http.NewRequest("POST", h.HTTP.URL+"/v1/videos", &body)
	if err != nil {
		h.t.Fatal(err)
	}
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+secret)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatalf("invalid video response %s", raw)
		}
	}
	return response.StatusCode, out
}

func mediaJobIDs(t *testing.T, h *accessHarness, b *browser) []string {
	t.Helper()
	list := h.want(b, "GET", "/api/v1/media-jobs", nil, nil, 200)
	ids := []string{}
	for _, item := range list["items"].([]any) {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	return ids
}

func TestMediaManagement(t *testing.T) {
	up := newVideoUpstream(t)
	h := newAccessHarness(t)
	owner := h.owner()

	projectA := createProject(h, owner, "Media Alpha")
	op := h.invite(owner, "media-op@example.com", "operator")
	viewer := h.invite(owner, "media-viewer@example.com", "operator")
	outsider := h.invite(owner, "media-outsider@example.com", "operator")
	users := h.want(owner, "GET", "/api/v1/users", nil, nil, 200)
	ids := map[string]map[string]any{}
	for _, item := range users["items"].([]any) {
		record := item.(map[string]any)
		ids[record["email"].(string)] = record
	}
	for _, email := range []string{"media-op@example.com", "media-viewer@example.com", "media-outsider@example.com"} {
		h.want(owner, "PATCH", "/api/v1/users/"+ids[email]["id"].(string), map[string]any{"access_scope": "assigned"}, etagHeader(ids[email]), 200)
	}
	addMember(h, owner, projectA, ids["media-op@example.com"]["id"].(string), "manager")
	addMember(h, owner, projectA, ids["media-viewer@example.com"]["id"].(string), "viewer")
	op = login(h, "media-op@example.com")
	viewer = login(h, "media-viewer@example.com")
	outsider = login(h, "media-outsider@example.com")

	_, slug := provisionVideo(t, h, op, up.URL+"/v1", projectA)
	key := h.want(op, "POST", "/api/v1/api-keys", map[string]any{
		"name": "media key", "scopes": []string{"inference"},
		"project_id": projectA, "allowed_routes": []string{slug},
	}, idem("media-key"), 201)
	secret := key["secret"].(string)
	h.refresh()

	status, created := h.videoCreate(slug, secret)
	if status != http.StatusCreated || created["status"] != "queued" {
		t.Fatalf("video create: %d %v", status, created)
	}
	status, created2 := h.videoCreate(slug, secret)
	if status != http.StatusCreated {
		t.Fatalf("second video create: %d %v", status, created2)
	}
	if up.createCalls.Load() != 2 {
		t.Fatalf("provider create calls %d, want 2", up.createCalls.Load())
	}

	jobIDs := mediaJobIDs(t, h, owner)
	if len(jobIDs) != 2 {
		t.Fatalf("owner media job list %v, want 2 jobs", jobIDs)
	}
	if got := mediaJobIDs(t, h, op); len(got) != 2 {
		t.Fatalf("manager media job list %v, want 2 jobs", got)
	}
	if got := mediaJobIDs(t, h, outsider); len(got) != 0 {
		t.Fatalf("out-of-scope principal must see no jobs, got %v", got)
	}
	jobID, job2ID := jobIDs[0], jobIDs[1]
	jobPath := "/api/v1/media-jobs/" + jobID
	job2Path := "/api/v1/media-jobs/" + job2ID

	h.want(outsider, "GET", jobPath, nil, nil, 404)
	h.want(outsider, "POST", jobPath+"/refresh", nil, idem("refresh-out"), 404)
	h.want(outsider, "DELETE", jobPath, nil, etagHeader(map[string]any{"etag": uuid.NewString()}), 404)

	detail := h.want(op, "GET", jobPath, nil, nil, 200)
	if detail["state"] != "queued" || detail["etag"] == nil {
		t.Fatalf("job detail %v", detail)
	}
	h.want(viewer, "GET", jobPath, nil, nil, 200)
	if status, out, _ := h.request(viewer, "POST", jobPath+"/refresh", nil, idem("refresh-viewer")); status != 403 {
		t.Fatalf("viewer refresh: %d %v", status, out)
	}
	if status, out, _ := h.request(viewer, "DELETE", jobPath, nil, etagHeader(detail)); status != 403 {
		t.Fatalf("viewer delete: %d %v", status, out)
	}
	if status, out, _ := h.request(viewer, "GET", jobPath+"/content?variant=video", nil, nil); status != 403 {
		t.Fatalf("viewer content: %d %v", status, out)
	}

	up.status.Store("completed")
	var hundred float32 = 100
	up.progress.Store(&hundred)
	refreshed := h.want(op, "POST", jobPath+"/refresh", nil, idem("refresh-1"), 200)
	if refreshed["state"] != "succeeded" || refreshed["content_available"] != true {
		t.Fatalf("refresh result %v", refreshed)
	}
	if up.getCalls.Load() != 1 {
		t.Fatalf("provider polls %d, want 1", up.getCalls.Load())
	}
	h.want(op, "POST", jobPath+"/refresh", nil, idem("refresh-2"), 200)
	if up.getCalls.Load() != 1 {
		t.Fatal("a terminal job refresh must not reach the provider")
	}
	if status, out, _ := h.request(op, "GET", job2Path+"/content?variant=video", nil, nil); status != 409 || problemCode(t, out) != "media_content_unavailable" {
		t.Fatalf("unfinished job content: %d %v", status, out)
	}

	if _, err := h.Pool.Exec(t.Context(),
		"UPDATE olp.media_jobs SET reconciliation_claim_id=$1, reconciliation_claimed_until=now()+interval '5 minutes' WHERE id=$2",
		uuid.NewString(), job2ID); err != nil {
		t.Fatal(err)
	}
	if status, out, _ := h.request(op, "POST", job2Path+"/refresh", nil, idem("refresh-busy")); status != 409 || problemCode(t, out) != "media_job_busy" {
		t.Fatalf("claimed job refresh: %d %v", status, out)
	}
	if up.getCalls.Load() != 1 {
		t.Fatal("a busy refresh must not reach the provider")
	}
	if _, err := h.Pool.Exec(t.Context(),
		"UPDATE olp.media_jobs SET reconciliation_claim_id=NULL, reconciliation_claimed_until=NULL WHERE id=$1", job2ID); err != nil {
		t.Fatal(err)
	}
	h.want(op, "POST", job2Path+"/refresh", nil, idem("refresh-clear"), 200)
	if up.getCalls.Load() != 2 {
		t.Fatalf("provider polls %d, want 2", up.getCalls.Load())
	}

	if status, out, _ := h.request(op, "GET", jobPath+"/content?variant=bogus", nil, nil); status != 400 {
		t.Fatalf("bad variant: %d %v", status, out)
	}
	spoolBefore := h.Media.Transport.Spool.UsedBytes()
	resp, body := h.do(op, "GET", jobPath+"/content?variant=video", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("content download: %d %s", resp.StatusCode, body)
	}
	if string(body) != up.contentBytes+"-video" {
		t.Fatalf("content body %q", body)
	}
	if disposition := resp.Header.Get("Content-Disposition"); !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, ".mp4") {
		t.Fatalf("content disposition %q", disposition)
	}
	resp.Body.Close()
	if used := h.Media.Transport.Spool.UsedBytes(); used != spoolBefore {
		t.Fatalf("spool bytes %d after download, want %d", used, spoolBefore)
	}
	if up.contentCalls.Load() != 1 {
		t.Fatalf("provider content calls %d, want 1", up.contentCalls.Load())
	}

	if status, out, _ := h.request(op, "DELETE", job2Path, nil, nil); status != 428 {
		t.Fatalf("delete without If-Match: %d %v", status, out)
	}
	if status, out, _ := h.request(op, "DELETE", job2Path, nil, map[string]string{"If-Match": `"` + uuid.NewString() + `"`}); status != 412 {
		t.Fatalf("delete with stale ETag: %d %v", status, out)
	}
	job2 := h.want(op, "GET", job2Path, nil, nil, 200)
	h.want(op, "DELETE", job2Path, nil, etagHeader(job2), 204)
	if up.deleteCalls.Load() != 1 {
		t.Fatalf("provider deletes %d, want 1", up.deleteCalls.Load())
	}
	deleted := h.want(op, "GET", job2Path, nil, nil, 200)
	if deleted["lifecycle"] != "deleted" {
		t.Fatalf("deleted job tombstone %v", deleted)
	}
	h.want(op, "DELETE", job2Path, nil, etagHeader(deleted), 204)
	if up.deleteCalls.Load() != 1 {
		t.Fatal("a deleted job must not reach the provider again")
	}

	rows, err := h.Pool.Query(t.Context(),
		"SELECT action FROM olp.Audit WHERE resource_type='media_job' AND resource_id=$1", jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actions := map[string]bool{}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions[action] = true
	}
	for _, want := range []string{"media_job.refresh", "media_job.content_download"} {
		if !actions[want] {
			t.Fatalf("audit actions %v missing %s", actions, want)
		}
	}
	rows, err = h.Pool.Query(t.Context(),
		"SELECT action FROM olp.Audit WHERE resource_type='media_job' AND resource_id=$1", job2ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	deleteAudited := false
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		if action == "media_job.delete" {
			deleteAudited = true
		}
	}
	if !deleteAudited {
		t.Fatal("media_job.delete audit is missing")
	}
	var leak int
	if err := h.Pool.QueryRow(t.Context(),
		"SELECT count(*) FROM olp.Audit WHERE resource_id LIKE '%'||$1||'%'", up.contentBytes).Scan(&leak); err != nil {
		t.Fatal(err)
	}
	if leak != 0 {
		t.Fatal("media content must never reach audit records")
	}
}
