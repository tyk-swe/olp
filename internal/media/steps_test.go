package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

// TestBFLWorkIsPolledThenFetched runs the Black Forest Labs work against a
// stand-in: the submission names a polling URL, the poll is pending once and
// then ready, and the signed image is fetched without the credential.
func TestBFLWorkIsPolledThenFetched(t *testing.T) {
	for _, caller := range []bool{false, true} {
		t.Run(fmt.Sprint(caller), func(t *testing.T) { testBFLWorkIsPolledThenFetched(t, caller) })
	}
}

func testBFLWorkIsPolledThenFetched(t *testing.T, caller bool) {
	var polls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/flux-2-pro":
			if r.Header.Get("X-Key") != "bfl-key-0123456789" {
				t.Error("submission lacks the key")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"task-1","polling_url":"` + server.URL + `/v1/get_result?id=task-1","cost":4.5}`))
		case r.URL.Path == "/v1/get_result":
			if r.Header.Get("X-Key") != "bfl-key-0123456789" {
				t.Error("poll lacks the key")
			}
			w.Header().Set("Content-Type", "application/json")
			if polls.Add(1) == 1 {
				w.Write([]byte(`{"id":"task-1","status":"Pending","progress":0.4}`))
				return
			}
			w.Write([]byte(`{"id":"task-1","status":"Ready","result":{"sample":"` + server.URL + `/delivery/sample.jpeg?sig=abc"}}`))
		case r.URL.Path == "/delivery/sample.jpeg":
			if r.Header.Get("X-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Error("the signed image URL received the credential")
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpeg-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	spool := testSpool(t, MinCapacityBytes)
	transport := &Transport{Client: policy.Client(10 * time.Second), Auth: connectors.NewAuth(policy), Egress: policy, Spool: spool, MaxResponseBytes: 1 << 20}
	call, failure := encodeBFLImage(imageRequest(1, "1024x1024", "b64_json"), "flux-2-pro")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	target := Target{Config: connectors.Config{Kind: "openai_compatible", AuthMode: "api_key", Endpoint: server.URL + "/v1", VendorID: "bfl"}, Model: "flux-2-pro", Secret: []byte("bfl-key-0123456789")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if caller {
		target.CallerCredential = true
		target.Config.CredentialSource = "caller"
	}
	result, transportFailure := transport.Do(ctx, target, call, imageRequest(1, "1024x1024", "b64_json"))
	if transportFailure != nil {
		t.Fatalf("BFL work failed: %+v", transportFailure)
	}
	if polls.Load() != 2 || result.Images == nil || len(result.Images.Images) != 1 || result.Images.Images[0].Handle == nil {
		t.Fatalf("result = %+v after %d polls", result.Images, polls.Load())
	}
	opened, err := spool.Open(*result.Images.Images[0].Handle)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.File.Close()
	var staged strings.Builder
	buf := make([]byte, 64)
	n, _ := opened.File.Read(buf)
	staged.Write(buf[:n])
	if staged.String() != "jpeg-bytes" {
		t.Fatalf("staged %q", staged.String())
	}
	// A work that strays outside its domains is refused before any request.
	if transport.stepAllowed("https://attacker.example/x", []string{"bfl.ai"}, server.URL+"/v1", true) || !transport.stepAllowed(server.URL+"/v1/get_result", []string{"bfl.ai"}, server.URL+"/v1", true) {
		t.Fatal("step addresses were not confined")
	}
}

func TestBFLModerationIsTheCallersError(t *testing.T) {
	if step, failure := nextBFLStep([]byte(`{"id":"task-1","status":"Request Moderated","result":null,"details":{"Moderation Reasons":["Violence"]}}`)); step != nil || failure == nil || failure.Status != 400 || failure.Code != "content_filter" {
		t.Fatalf("moderated work: %+v %+v", step, failure)
	}
	if _, failure := nextBFLStep([]byte(`{"id":"task-1","status":"Error"}`)); failure == nil || failure.Status != 502 {
		t.Fatalf("failed work: %+v", failure)
	}
}

// TestAssemblyAITranscriptIsPolledThenDeleted runs AssemblyAI's work against a
// stand-in: the audio is uploaded, a transcript submitted and polled until
// complete, and the transcript then deleted.
func TestAssemblyAITranscriptIsPolledThenDeleted(t *testing.T) {
	var polls, deletes atomic.Int32
	var submitted string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "assemblyai-key-0123456789" {
			t.Errorf("%s %s lacks the key", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/upload":
			if body, _ := io.ReadAll(r.Body); string(body) != "RIFFaudio" {
				t.Errorf("uploaded %q", body)
			}
			w.Write([]byte(`{"upload_url":"https://cdn.assemblyai.com/upload/abc"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/transcript":
			body, _ := io.ReadAll(r.Body)
			submitted = string(body)
			w.Write([]byte(`{"id":"t-1","status":"queued"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/transcript/t-1":
			if polls.Add(1) == 1 {
				w.Write([]byte(`{"id":"t-1","status":"processing"}`))
				return
			}
			w.Write([]byte(`{"id":"t-1","status":"completed","text":"Hello world.","language_code":"en","audio_duration":3,"words":[{"text":"Hello","start":100,"end":500},{"text":"world.","start":600,"end":1000}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/transcript/t-1":
			deletes.Add(1)
			w.Write([]byte(`{"id":"t-1","status":"completed","text":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	spool := testSpool(t, MinCapacityBytes)
	audio, err := spool.Put(t.Context(), Upload{Filename: "hello.wav", ContentType: "audio/wav", MaximumLength: 1 << 10, Body: strings.NewReader("RIFFaudio")})
	if err != nil {
		t.Fatal(err)
	}
	transport := &Transport{Client: policy.Client(10 * time.Second), Auth: connectors.NewAuth(policy), Egress: policy, Spool: spool, MaxResponseBytes: 1 << 20}
	cfg := connectors.Config{Kind: "openai_compatible", AuthMode: "api_key", Endpoint: server.URL, VendorID: "assemblyai"}
	verbose := "verbose_json"
	language := "en"
	request := &Request{Op: OpTranscription, Route: "route", Format: &verbose, Language: &language, File: &Part{Handle: audio.Handle, ContentType: "audio/wav"}}
	call, failure := encodeAssemblyAI(request, cfg, "universal-3-5-pro")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, transportFailure := transport.Do(ctx, Target{Config: cfg, Model: "universal-3-5-pro", Secret: []byte("assemblyai-key-0123456789")}, call, request)
	if transportFailure != nil {
		t.Fatalf("AssemblyAI work failed: %+v", transportFailure)
	}
	if submitted != `{"audio_url":"https://cdn.assemblyai.com/upload/abc","language_code":"en","speech_models":["universal-3-5-pro"]}` || polls.Load() != 2 || deletes.Load() != 1 {
		t.Fatalf("submitted %s, polled %d times, deleted %d times", submitted, polls.Load(), deletes.Load())
	}
	transcript := result.Transcription
	if transcript.Text != "Hello world." || *transcript.DurationSeconds != 3 {
		t.Fatalf("transcript = %+v", transcript)
	}
	rendered, _ := EncodeTranscriptionJSON(transcript)
	if !strings.Contains(string(rendered), `"start":0.1`) || !strings.Contains(string(rendered), `"duration":3`) {
		t.Fatalf("verbose_json client receives %s", rendered)
	}
}

func TestCallerStepsCannotForwardCredentialsToAnotherOrigin(t *testing.T) {
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	transport := &Transport{Egress: policy}
	for _, address := range []string{"http://127.0.0.1:9002/poll", "https://vendor.example/poll"} {
		called := false
		_, failure := transport.step(t.Context(), &Step{URL: address, Credentials: true}, &UpstreamCall{StepDomains: []string{"vendor.example"}}, func(*http.Request) (*http.Response, error) { called = true; return nil, nil }, Target{CallerCredential: true, Config: connectors.Config{Endpoint: "http://127.0.0.1:9001/api"}, Secret: []byte("caller-private")})
		if failure == nil || called || strings.Contains(failure.Detail, "caller-private") {
			t.Fatalf("forwarded across origins: %+v", failure)
		}
	}
}
