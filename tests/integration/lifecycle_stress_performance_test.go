//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This local native reference checks original media bytes, accepted-work
// identity and each provider status independently of the production codecs.
// The gateway and authorized relay are measured against the same fixture.
const (
	stressImageBytes   = 4 << 20
	stressContentBytes = 1 << 20
	stressPrompt       = "lifecycle stress image and durable job"
	stressEvents       = 64
)

type stressVideoFixture struct {
	*httptest.Server
	imageDigest string
	content     []byte
	mu          sync.Mutex
	stages      map[string]int
	posts       atomic.Int64
	creates     atomic.Int64
	gets        atomic.Int64
	contents    atomic.Int64
}

func stressDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func stressImage() []byte {
	image := make([]byte, stressImageBytes)
	for i := range image {
		image[i] = byte(i*37 + 19)
	}
	return image
}

func newStressVideoFixture(t *testing.T, image []byte) *stressVideoFixture {
	t.Helper()
	f := &stressVideoFixture{imageDigest: stressDigest(image), stages: map[string]int{}}
	f.content = make([]byte, stressContentBytes)
	for i := range f.content {
		f.content[i] = byte(i*17 + 3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"id": vendorModel, "object": "model"}}})
	})
	mux.HandleFunc("POST /v1/videos", func(w http.ResponseWriter, r *http.Request) {
		f.posts.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			http.Error(w, "credential", http.StatusUnauthorized)
			return
		}
		reader, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "multipart", http.StatusBadRequest)
			return
		}
		fields := map[string]string{}
		fileCount := 0
		for {
			part, nextErr := reader.NextPart()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				http.Error(w, "partial multipart", http.StatusBadRequest)
				return
			}
			name := part.FormName()
			if name == "input_reference" {
				fileCount++
				if part.FileName() != "source.png" || part.Header.Get("Content-Type") != "image/png" {
					t.Error("upstream image identity changed")
					http.Error(w, "image identity", http.StatusBadRequest)
					return
				}
				hash := sha256.New()
				n, copyErr := io.Copy(hash, io.LimitReader(part, stressImageBytes+1))
				if copyErr != nil || n != stressImageBytes || hex.EncodeToString(hash.Sum(nil)) != f.imageDigest {
					t.Error("upstream original image bytes changed")
					http.Error(w, "image bytes", http.StatusBadRequest)
					return
				}
			} else {
				raw, readErr := io.ReadAll(io.LimitReader(part, 1<<16))
				if readErr != nil || len(raw) == 1<<16 || name == "" {
					http.Error(w, "field", http.StatusBadRequest)
					return
				}
				if _, exists := fields[name]; exists {
					t.Error("duplicate video field")
					http.Error(w, "duplicate", http.StatusBadRequest)
					return
				}
				fields[name] = string(raw)
			}
			part.Close()
		}
		if fileCount != 1 || len(fields) != 3 || fields["model"] != vendorModel || fields["prompt"] != stressPrompt || fields["seconds"] != "4" {
			t.Errorf("original video controls changed: %v, files=%d", fields, fileCount)
			http.Error(w, "controls", http.StatusBadRequest)
			return
		}
		id := fmt.Sprintf("vid-stress-%d", f.creates.Add(1))
		f.mu.Lock()
		f.stages[id] = 0
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": id, "object": "video", "model": vendorModel, "status": "queued", "created_at": 1, "seconds": "4"})
	})
	mux.HandleFunc("GET /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		stage, ok := f.stages[id]
		if ok {
			stage++
			f.stages[id] = stage
		}
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.gets.Add(1)
		status, progress := "in_progress", 50
		if stage >= 2 {
			status, progress = "completed", 100
		}
		writeJSON(w, map[string]any{"id": id, "object": "video", "model": vendorModel, "status": status, "progress": progress, "created_at": 1, "seconds": "4"})
	})
	mux.HandleFunc("GET /v1/videos/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		stage, ok := f.stages[r.PathValue("id")]
		f.mu.Unlock()
		if !ok || stage < 2 || r.URL.Query().Get("variant") != "video" {
			http.Error(w, "content unavailable", http.StatusConflict)
			return
		}
		f.contents.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprint(len(f.content)))
		_, _ = w.Write(f.content)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func stressRelay(t *testing.T, provider *stressVideoFixture) *httptest.Server {
	t.Helper()
	target, err := url.Parse(provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	original := proxy.Director
	proxy.Director = func(r *http.Request) {
		original(r)
		r.Header.Set("Authorization", "Bearer "+vendorSecret)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer lifecycle-stress-reference-key-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func stressMultipart(t *testing.T, model string, image []byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, field := range [][2]string{{"model", model}, {"prompt", stressPrompt}, {"seconds", "4"}} {
		if err := form.WriteField(field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="input_reference"; filename="source.png"`)
	header.Set("Content-Type", "image/png")
	part, err := form.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), form.FormDataContentType()
}

func stressProvisionVideo(t *testing.T, h *accessHarness, owner *browser, endpoint string) string {
	t.Helper()
	path := "/api/v3/providers"
	provider := h.want(owner, http.MethodPost, path, map[string]any{
		"name": "Lifecycle stress video reference", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai", "auth_mode": "api_key", "endpoint": endpoint},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	path += "/" + provider["id"].(string)
	if probe := h.want(owner, http.MethodPost, path+"/probe", nil, etagHeader(provider), http.StatusOK); probe["succeeded"] != true {
		t.Fatalf("video probe failed: %v", probe)
	}
	certifyVideoSeeded(t, h, provider["id"].(string))
	provider = h.want(owner, http.MethodGet, path, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, path+"/activate", nil, withMatch(provider, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	slug := "stress-video-" + uuid.NewString()[:8]
	draft := map[string]any{
		"slug": slug, "overall_timeout_ms": 30000, "max_attempts": 1,
		"operations": []string{"video_create", "video_get", "video_content", "video_delete"},
		"targets":    []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 20000}},
	}
	raw := os.Getenv("OLP_LIFECYCLE_ROUTE_FIDELITY")
	if raw == "" {
		raw = `{"mode":"legacy"}`
	}
	var fidelity map[string]any
	if json.Unmarshal([]byte(raw), &fidelity) != nil || len(fidelity) != 1 || (fidelity["mode"] != "legacy" && fidelity["mode"] != "strict") {
		t.Fatal("video benchmark route fidelity must contain exactly legacy or strict mode")
	}
	draft["fidelity"] = fidelity
	created := h.want(owner, http.MethodPost, "/api/v3/route-drafts", draft, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v3/route-drafts/"+created["id"].(string)+"/activate", nil, withMatch(created, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	return slug
}

type stressSample struct {
	elapsed, firstByte, cancellation                                    time.Duration
	gaps, eventRTT, eventJitter                                         []time.Duration
	accepted, partial, retrieved, content, events, uploaded, downloaded int
}

func stressJSONRequest(ctx context.Context, client *http.Client, endpoint, secret, method, path string, body []byte, contentType string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// The gateway's accepted-work receipt is 201; the native relay forwards
	// the provider's 200 without relabelling it.
	if resp.StatusCode != http.StatusOK && !(method == http.MethodPost && path == "/v1/videos" && resp.StatusCode == http.StatusCreated) {
		return nil, fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func stressCreate(ctx context.Context, client *http.Client, endpoint, secret, model string, body []byte, contentType string) (string, error) {
	doc, err := stressJSONRequest(ctx, client, endpoint, secret, http.MethodPost, "/v1/videos", body, contentType)
	if err != nil {
		return "", err
	}
	id, ok := doc["id"].(string)
	if !ok || id == "" || doc["object"] != "video" || doc["model"] != model || doc["status"] != "queued" || doc["created_at"] != float64(1) || doc["seconds"] != "4" {
		return "", fmt.Errorf("accepted-work receipt changed: %v", doc)
	}
	return id, nil
}

func stressPoll(ctx context.Context, client *http.Client, endpoint, secret, id string, stage int) error {
	doc, err := stressJSONRequest(ctx, client, endpoint, secret, http.MethodGet, "/v1/videos/"+id, nil, "")
	if err != nil {
		return err
	}
	status, progress := "in_progress", float64(50)
	if stage == 2 {
		status, progress = "completed", 100
	}
	if doc["id"] != id || doc["object"] != "video" || doc["status"] != status || doc["progress"] != progress || doc["seconds"] != "4" {
		return fmt.Errorf("partial/terminal retrieval changed: %v", doc)
	}
	return nil
}

func stressContent(ctx context.Context, client *http.Client, endpoint, secret, id string, fixture *stressVideoFixture, slow bool) (stressSample, error) {
	var sample stressSample
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/videos/"+id+"/content?variant=video", nil)
	if err != nil {
		return sample, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return sample, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" {
		return sample, fmt.Errorf("content HTTP %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	hash := sha256.New()
	buf := make([]byte, 8<<10)
	lastRead := time.Time{}
	for {
		if slow {
			time.Sleep(250 * time.Microsecond)
		}
		readStart := time.Now()
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if sample.firstByte == 0 {
				sample.firstByte = time.Since(start)
			}
			if !lastRead.IsZero() {
				sample.gaps = append(sample.gaps, readStart.Sub(lastRead))
			}
			lastRead = time.Now()
			_, _ = hash.Write(buf[:n])
			sample.downloaded += n
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return sample, readErr
		}
	}
	sample.elapsed = time.Since(start)
	if sample.downloaded != stressContentBytes || hex.EncodeToString(hash.Sum(nil)) != stressDigest(fixture.content) {
		return sample, fmt.Errorf("retrieved media byte identity changed")
	}
	sample.retrieved, sample.content = 1, 1
	return sample, nil
}

func stressVideoCycle(ctx context.Context, client *http.Client, endpoint, secret, model string, body []byte, contentType string, fixture *stressVideoFixture) (stressSample, string, error) {
	start := time.Now()
	id, err := stressCreate(ctx, client, endpoint, secret, model, body, contentType)
	if err != nil {
		return stressSample{}, "", err
	}
	for stage := 1; stage <= 2; stage++ {
		if err := stressPoll(ctx, client, endpoint, secret, id, stage); err != nil {
			return stressSample{}, "", err
		}
	}
	sample, err := stressContent(ctx, client, endpoint, secret, id, fixture, false)
	if err != nil {
		return stressSample{}, "", err
	}
	sample.elapsed = time.Since(start)
	sample.accepted, sample.partial, sample.retrieved, sample.content, sample.uploaded = 1, 1, 2, 1, stressImageBytes
	return sample, id, nil
}

func stressDuplex(ctx context.Context, h *accessHarness, fixture *lifecycleFixture, client *http.Client, endpoint, secret, model string, relay bool) (stressSample, error) {
	result, err := lifecycleRequest(ctx, h, fixture, client, endpoint, secret, model, "duplex_64", relay)
	if err != nil {
		return stressSample{}, err
	}
	if len(result.roundTrips) != stressEvents {
		return stressSample{}, fmt.Errorf("duplex event count %d", len(result.roundTrips))
	}
	if result.cancellation <= 0 {
		return stressSample{}, fmt.Errorf("provider closure was not observed")
	}
	sample := stressSample{elapsed: result.elapsed, cancellation: result.cancellation, eventRTT: result.roundTrips, events: stressEvents}
	for i := 1; i < len(result.roundTrips); i++ {
		delta := result.roundTrips[i] - result.roundTrips[i-1]
		if delta < 0 {
			delta = -delta
		}
		sample.eventJitter = append(sample.eventJitter, delta)
	}
	return sample, nil
}

type stressRun struct {
	Name            string             `json:"name"`
	Repetition      int                `json:"repetition"`
	Samples         int                `json:"samples"`
	Succeeded       int                `json:"succeeded"`
	Rejected        int                `json:"rejected"`
	Incomplete      int                `json:"incomplete"`
	Ambiguous       int                `json:"ambiguous"`
	Dispatches      int                `json:"dispatches"`
	Accepted        int                `json:"accepted"`
	Partial         int                `json:"partial"`
	Retrieved       int                `json:"retrieved"`
	Content         int                `json:"content"`
	Events          int                `json:"events"`
	RTTObservations int                `json:"rtt_observations"`
	JitterSamples   int                `json:"jitter_observations"`
	UploadedBytes   int                `json:"uploaded_bytes"`
	DownloadedBytes int                `json:"downloaded_bytes"`
	Metrics         map[string]float64 `json:"metrics"`
}

func stressCPU() int64 {
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return usage.Utime.Nano() + usage.Stime.Nano()
}

func stressPercentile(values []time.Duration, percentile int) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return float64(values[(len(values)-1)*percentile/100]) / float64(time.Microsecond)
}

func stressNegativeControls(t *testing.T, h *accessHarness, fixture *stressVideoFixture, slug, key string, image []byte) {
	t.Helper()
	before := fixture.posts.Load()
	tooLarge := make([]byte, 21<<20)
	copy(tooLarge, image)
	body, contentType := stressMultipart(t, slug, tooLarge)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/videos", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", contentType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge || fixture.posts.Load() != before || h.Media.Transport.Spool.UsedBytes() != 0 {
		t.Fatalf("oversized media was dispatched or retained: status=%d", response.StatusCode)
	}
	t.Logf("LIFECYCLE_STRESS_NEGATIVE %s", `{"name":"oversize_upload","path":"gateway","admitted":0,"rejected":1,"provider_dispatches":0,"spool_final_bytes":0}`)

	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/v1/videos", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=cancelled-upload")
	defer cancel()
	defer writer.CloseWithError(context.Canceled)
	requestDone := make(chan *http.Response, 1)
	go func() {
		response, _ := http.DefaultClient.Do(request)
		requestDone <- response
	}()
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = io.WriteString(writer, "--cancelled-upload\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n"+slug+"\r\n--cancelled-upload\r\nContent-Disposition: form-data; name=\"input_reference\"; filename=\"source.png\"\r\nContent-Type: image/png\r\n\r\n")
		_, _ = writer.Write(image[:256<<10])
	}()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled-upload control never reached the parser")
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.Media.Transport.Spool.UsedBytes() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.Media.Transport.Spool.UsedBytes() == 0 {
		t.Fatal("cancelled-upload control did not stage bytes")
	}
	cancel()
	writer.CloseWithError(context.Canceled)
	select {
	case response := <-requestDone:
		if response != nil {
			response.Body.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled upload did not terminate")
	}
	deadline = time.Now().Add(2 * time.Second)
	for h.Media.Transport.Spool.UsedBytes() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fixture.posts.Load() != before || h.Media.Transport.Spool.UsedBytes() != 0 {
		t.Fatal("cancelled upload dispatched or leaked spool reservation")
	}
	t.Logf("LIFECYCLE_STRESS_NEGATIVE %s", `{"name":"cancelled_upload","path":"gateway","admitted":0,"rejected":1,"provider_dispatches":0,"spool_final_bytes":0}`)

	// Hold one authenticated parser mid-file, then submit another full request
	// with the same key. The second must be a local capacity refusal, and the
	// held upload must release its reservation when cancelled.
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	firstReader, firstWriter := io.Pipe()
	defer cancelFirst()
	defer firstWriter.CloseWithError(context.Canceled)
	firstRequest, err := http.NewRequestWithContext(firstCtx, http.MethodPost, h.HTTP.URL+"/v1/videos", firstReader)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest.Header.Set("Authorization", "Bearer "+key)
	firstRequest.Header.Set("Content-Type", "multipart/form-data; boundary=held-upload")
	firstResponse := make(chan *http.Response, 1)
	go func() {
		response, _ := http.DefaultClient.Do(firstRequest)
		firstResponse <- response
	}()
	writeDone = make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = io.WriteString(firstWriter, "--held-upload\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n"+slug+"\r\n--held-upload\r\nContent-Disposition: form-data; name=\"input_reference\"; filename=\"source.png\"\r\nContent-Type: image/png\r\n\r\n")
		_, _ = firstWriter.Write(image[:256<<10])
	}()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		cancelFirst()
		firstWriter.CloseWithError(context.Canceled)
		t.Fatal("held upload never reached the parser")
	}
	deadline = time.Now().Add(2 * time.Second)
	for h.Media.Transport.Spool.UsedBytes() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.Media.Transport.Spool.UsedBytes() == 0 {
		cancelFirst()
		firstWriter.CloseWithError(context.Canceled)
		t.Fatal("held upload did not reserve spool bytes")
	}
	secondBody, secondType := stressMultipart(t, slug, image)
	secondRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/videos", bytes.NewReader(secondBody))
	if err != nil {
		t.Fatal(err)
	}
	secondRequest.Header.Set("Authorization", "Bearer "+key)
	secondRequest.Header.Set("Content-Type", secondType)
	second, err := http.DefaultClient.Do(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, second.Body)
	second.Body.Close()
	cancelFirst()
	firstWriter.CloseWithError(context.Canceled)
	select {
	case response := <-firstResponse:
		if response != nil {
			response.Body.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled held upload did not terminate")
	}
	deadline = time.Now().Add(2 * time.Second)
	for h.Media.Transport.Spool.UsedBytes() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if second.StatusCode != http.StatusServiceUnavailable || fixture.posts.Load() != before || h.Media.Transport.Spool.UsedBytes() != 0 {
		t.Fatalf("parser contention did not refuse locally and release bytes: status=%d", second.StatusCode)
	}
	t.Logf("LIFECYCLE_STRESS_NEGATIVE %s", `{"name":"parser_contention","path":"gateway","admitted":0,"rejected":1,"provider_dispatches":0,"spool_final_bytes":0}`)
}

func TestLifecycleStressPerformance(t *testing.T) {
	h := newAccessHarness(t)
	var postgres string
	var tls bool
	if err := h.Pool.QueryRow(t.Context(), "SHOW server_version_num").Scan(&postgres); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()),false)").Scan(&tls); err != nil {
		t.Fatal(err)
	}
	setup, _ := json.Marshal(map[string]any{"postgresql_server_version": postgres, "postgresql_tls": tls})
	t.Logf("LIFECYCLE_STRESS_SETUP %s", setup)
	image := stressImage()
	video := newStressVideoFixture(t, image)
	owner := h.owner()
	videoSlug := stressProvisionVideo(t, h, owner, video.URL+"/v1")
	videoKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "stress media key", "scopes": []string{"inference"}, "allowed_routes": []string{videoSlug}}, map[string]string{"Idempotency-Key": "stress-media-key"}, 201)["secret"].(string)
	videoKey2 := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "stress media key 2", "scopes": []string{"inference"}, "allowed_routes": []string{videoSlug}}, map[string]string{"Idempotency-Key": "stress-media-key-2"}, 201)["secret"].(string)
	h.refresh()
	videoRelay := stressRelay(t, video)
	duplexHarness := newAccessHarness(t)
	duplex := newLifecycleFixture(t)
	duplexSlug, duplexKey := lifecycleProvision(t, duplexHarness, duplex.URL)
	duplex.measuring.Store(true)
	duplexRelay := lifecycleRelay(t, duplex)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 32}, Timeout: 45 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	mediaBodies := map[string][]byte{}
	mediaTypes := map[string]string{}
	mediaBodies["gateway"], mediaTypes["gateway"] = stressMultipart(t, videoSlug, image)
	mediaBodies["relay"], mediaTypes["relay"] = stressMultipart(t, vendorModel, image)
	repetitions, samples := 1, 2
	if testing.Short() {
		samples = 1
	}
	if os.Getenv("OLP_LIFECYCLE_STRESS_MEASURE") == "1" {
		repetitions, samples = 3, 8
	}
	for _, workload := range []string{"media_cycle_4m", "media_slow_content_1m", "duplex_jitter_64"} {
		concurrencies := []int{1, 2}
		if workload == "duplex_jitter_64" {
			concurrencies = []int{1, 4}
		}
		for _, concurrency := range concurrencies {
			for _, path := range []string{"relay", "gateway"} {
				mediaEndpoint, mediaModel := h.HTTP.URL, videoSlug
				duplexEndpoint, duplexSecret, duplexModel := duplexHarness.HTTP.URL, duplexKey, duplexSlug
				if path == "relay" {
					mediaEndpoint, mediaModel = videoRelay.URL, vendorModel
					duplexEndpoint, duplexSecret, duplexModel = duplexRelay.URL, "lifecycle-reference-key", vendorModel
				}
				mediaSecret := func(worker int) string {
					if path == "relay" {
						return fmt.Sprintf("lifecycle-stress-reference-key-%d", worker)
					}
					if worker == 0 {
						return videoKey
					}
					return videoKey2
				}
				for repetition := range repetitions {
					seedIDs := make([]string, concurrency)
					if workload == "media_slow_content_1m" {
						for worker := range concurrency {
							_, id, err := stressVideoCycle(t.Context(), client, mediaEndpoint, mediaSecret(worker), mediaModel, mediaBodies[path], mediaTypes[path], video)
							if err != nil {
								t.Fatal(err)
							}
							seedIDs[worker] = id
						}
					}
					measureOne := func(ctx context.Context, worker int) (stressSample, error) {
						switch workload {
						case "media_cycle_4m":
							sample, _, err := stressVideoCycle(ctx, client, mediaEndpoint, mediaSecret(worker), mediaModel, mediaBodies[path], mediaTypes[path], video)
							return sample, err
						case "media_slow_content_1m":
							return stressContent(ctx, client, mediaEndpoint, mediaSecret(worker), seedIDs[worker], video, true)
						default:
							return stressDuplex(ctx, duplexHarness, duplex, client, duplexEndpoint, duplexSecret, duplexModel, path == "relay")
						}
					}
					if _, err := measureOne(t.Context(), 0); err != nil {
						t.Fatalf("%s warmup: %v", workload, err)
					}
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					var peakHeap, peakSpool atomic.Uint64
					peakHeap.Store(before.HeapAlloc)
					stop, stopped := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(stopped)
						ticker := time.NewTicker(time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-stop:
								return
							case <-ticker.C:
								var m runtime.MemStats
								runtime.ReadMemStats(&m)
								peakHeap.Store(max(peakHeap.Load(), m.HeapAlloc))
								peakSpool.Store(max(peakSpool.Load(), uint64(h.Media.Transport.Spool.UsedBytes())))
							}
						}
					}()
					beforeDispatches := video.posts.Load() + video.gets.Load() + video.contents.Load() + duplex.dispatches.Load()
					cpu, started := stressCPU(), time.Now()
					results := make([]stressSample, samples)
					errs := make(chan error, samples)
					var workers sync.WaitGroup
					for worker := range concurrency {
						workers.Go(func() {
							for i := worker; i < samples; i += concurrency {
								ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
								sample, err := measureOne(ctx, worker)
								cancel()
								if err != nil {
									errs <- err
								}
								results[i] = sample
							}
						})
					}
					workers.Wait()
					elapsed := time.Since(started)
					cpu = stressCPU() - cpu
					dispatches := video.posts.Load() + video.gets.Load() + video.contents.Load() + duplex.dispatches.Load() - beforeDispatches
					close(stop)
					<-stopped
					runtime.ReadMemStats(&after)
					peakHeap.Store(max(peakHeap.Load(), after.HeapAlloc))
					close(errs)
					for err := range errs {
						t.Fatal(err)
					}
					observations := map[string][]time.Duration{"latency": {}, "first-byte": {}, "inter-read-gap": {}, "event-rtt": {}, "event-jitter": {}, "cancellation": {}}
					run := stressRun{Name: fmt.Sprintf("%s/c%d/%s", workload, concurrency, path), Repetition: repetition, Samples: samples, Succeeded: samples, Dispatches: int(dispatches), Metrics: map[string]float64{
						"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / float64(samples), "process-cpu-ns/op": float64(cpu) / float64(samples),
						"B/op": float64(after.TotalAlloc-before.TotalAlloc) / float64(samples), "allocs/op": float64(after.Mallocs-before.Mallocs) / float64(samples),
						"sampled-heap-growth-B": float64(peakHeap.Load() - before.HeapAlloc), "sampled-spool-peak-B": float64(peakSpool.Load()),
						"spool-final-B": float64(h.Media.Transport.Spool.UsedBytes()),
					}}
					for _, result := range results {
						observations["latency"] = append(observations["latency"], result.elapsed)
						observations["first-byte"] = append(observations["first-byte"], result.firstByte)
						observations["inter-read-gap"] = append(observations["inter-read-gap"], result.gaps...)
						observations["event-rtt"] = append(observations["event-rtt"], result.eventRTT...)
						observations["event-jitter"] = append(observations["event-jitter"], result.eventJitter...)
						observations["cancellation"] = append(observations["cancellation"], result.cancellation)
						run.Accepted += result.accepted
						run.Partial += result.partial
						run.Retrieved += result.retrieved
						run.Content += result.content
						run.Events += result.events
						run.RTTObservations += len(result.eventRTT)
						run.JitterSamples += len(result.eventJitter)
						run.UploadedBytes += result.uploaded
						run.DownloadedBytes += result.downloaded
					}
					categories := []string{"latency", "first-byte", "inter-read-gap"}
					if workload == "duplex_jitter_64" {
						categories = []string{"latency", "event-rtt", "event-jitter", "cancellation"}
					}
					for _, category := range categories {
						for _, percentile := range []int{50, 95, 99} {
							run.Metrics[fmt.Sprintf("%s-p%d-us", category, percentile)] = stressPercentile(observations[category], percentile)
						}
					}
					if run.Dispatches != map[string]int{"media_cycle_4m": 4 * samples, "media_slow_content_1m": samples, "duplex_jitter_64": samples}[workload] || run.Metrics["spool-final-B"] != 0 {
						t.Fatalf("work conservation/spool changed: %+v", run)
					}
					raw, _ := json.Marshal(run)
					t.Logf("LIFECYCLE_STRESS_MEASUREMENT %s", raw)
				}
			}
		}
	}
	stressNegativeControls(t, h, video, videoSlug, videoKey, image)
}
