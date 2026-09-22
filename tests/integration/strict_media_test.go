//go:build integration

package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providers"
)

// Local fixtures cannot claim official OpenAI media certification. Seed only
// the disposable installation's certified tuple after public provider creation
// and nonbillable probe; production certification remains official-host only.
func certifyStrictMediaFixture(t *testing.T, h *accessHarness, providerID, operation, mode string) {
	t.Helper()
	var raw []byte
	if err := h.Pool.QueryRow(t.Context(), "SELECT configuration FROM olp_go.providers WHERE id=$1", providerID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var cfg providers.Configuration
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	parts := []any{cfg.Kind, cfg.AuthMode, cfg.Endpoint, cfg.CloudRegion, cfg.CloudProject, cfg.Deployment, cfg.APIVersion, cfg.Options.CredentialHeaders, cfg.Options.ParameterDefaults, cfg.Options.Models, cfg.Options.VendorID}
	parts = append(parts, cfg.ProfileID, cfg.ProfileRevision, cfg.Options.SemanticHeaders, cfg.Options.QuerySettings, cfg.Options.OperationDefaults, cfg.Options.Bindings)
	if cfg.Options.Network != nil {
		parts = append(parts, cfg.Options.Network)
	}
	transport, _ := json.Marshal(parts)
	transportSum := sha256.Sum256(transport)
	transportFP := hex.EncodeToString(transportSum[:])[:32]
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT credential_id::text FROM olp_go.provider_slots WHERE provider_id=$1 AND is_default", providerID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	credentialFP := transportFP + ":" + credentialID
	tuples := []string{}
	for _, c := range [][3]string{{operation, "openai", mode}} {
		encoded, _ := json.Marshal([]string{vendorModel, c[0], c[1], c[2]})
		tuples = append(tuples, string(encoded))
	}
	sort.Strings(tuples)
	encodedTuples, _ := json.Marshal(tuples)
	tupleSum := sha256.Sum256(encodedTuples)
	validatedFP := credentialFP + ":" + hex.EncodeToString(tupleSum[:])
	now := time.Now().UTC()
	capabilities, _ := json.Marshal([]map[string]any{{"operation": operation, "surface": "openai", "mode": mode, "source": "certified", "certified_at": now.Format(time.RFC3339Nano), "credential_fingerprint": credentialFP}})
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.provider_models SET capabilities=$1 WHERE provider_id=$2 AND upstream_model=$3", capabilities, providerID, vendorModel); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.provider_slots SET validated_at=$1, validated_fingerprint=$2 WHERE provider_id=$3 AND is_default", now, validatedFP, providerID); err != nil {
		t.Fatal(err)
	}
}

func publishStrictMediaFixture(t *testing.T, h *accessHarness, owner *browser, fixture *operationFixture, operation, mode string, defaults ...map[string]any) (string, string) {
	t.Helper()
	configuration := map[string]any{"kind": "openai_compatible", "profile_id": "compatible-chat", "profile_revision": "1", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1"}
	if len(defaults) > 0 && len(defaults[0]) > 0 {
		profile, err := connectors.LookupProfile("compatible-chat", "1")
		if err != nil {
			t.Fatal(err)
		}
		configuration["options"] = map[string]any{"operation_defaults": map[string]any{operation: map[string]any{"dialect": profile.OperationDialect(operation), "values": defaults[0]}}}
	}
	provider := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name":          "Strict media fixture " + uuid.NewString(),
		"configuration": configuration,
		"model":         vendorModel, "credential": vendorSecret,
	}, idem(uuid.NewString()), 201)
	path := "/api/v3/providers/" + provider["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(provider), 200)
	if probe["succeeded"] != true {
		t.Fatalf("fixture model probe=%v", probe)
	}
	certifyStrictMediaFixture(t, h, provider["id"].(string), operation, mode)
	provider = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "strict-media-" + uuid.NewString()
	draftInput := fidelityDraft(slug, provider["id"])
	draftInput["operations"] = []string{operation}
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "Strict media fixture", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

// This exercises the public management publication and inference surfaces
// against a real encrypted installation and scripted provider. The fixture
// asserts source bytes independently of the media encoder.
func TestStrictMediaPublicNativeSourcesAndAssets(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	t.Run("image JSON source and result", func(t *testing.T) {
		result := `{"created":1,"data":[{"url":"https://asset.example/source"}],"native_number":9007199254740993}`
		fixture := newOperationFixture(t, "/v1/images/generations", result)
		slug, key := publishStrictMediaFixture(t, h, owner, fixture, "image_generation", "unary", map[string]any{"quality": "high"})
		body := `{"model":"` + slug + `","native":{"whole":9007199254740993,"signed_zero":-0},"prompt":"native image"}`
		status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/images/generations", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || string(raw) != result {
			t.Fatalf("native image status=%d body=%s", status, raw)
		}
		calls := fixture.snapshot()
		want := `{"model":"` + vendorModel + `","native":{"whole":9007199254740993,"signed_zero":-0},"prompt":"native image","quality":"high"}`
		if len(calls) != 1 || string(calls[0].body) != want {
			t.Fatalf("native image request=%+v", calls)
		}
		preview := h.list(owner, "POST", "/api/v3/routing/simulate", map[string]any{
			"operation": map[string]any{"operation": "image_generation", "request": map[string]any{"model": slug, "prompt": "private-inspector-prompt", "native": map[string]any{"opaque": "private-native-marker"}}},
			"surface":   "openai", "mode": "unary", "seed": "strict-media-inspector",
		}, nil, 200)
		if len(preview) != 1 {
			t.Fatalf("inspector decisions=%v", preview)
		}
		decision := preview[0].(map[string]any)
		interaction := decision["interaction"].(map[string]any)
		if decision["eligible"] != true || interaction["status"] != "admitted" || interaction["class"] != "native_identity" {
			t.Fatalf("strict media inspection=%v", decision)
		}
		fields := interaction["effective_request"].(map[string]any)["fields"].([]any)
		origins := map[string]string{}
		for _, raw := range fields {
			field := raw.(map[string]any)
			origins[field["field"].(string)] = field["origin"].(string)
		}
		if origins["/model"] != "identity_binding" || origins["/quality"] != "provider_default" || origins["/prompt"] != "caller" {
			t.Fatalf("media inspector provenance=%v", origins)
		}
		encoded, _ := json.Marshal(decision)
		if bytes.Contains(encoded, []byte("private-inspector-prompt")) || bytes.Contains(encoded, []byte("private-native-marker")) || len(fixture.snapshot()) != 1 {
			t.Fatalf("inspector exposed or dispatched native source: %s", encoded)
		}
		status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/images/generations", key, strings.NewReader(`{"model":"`+slug+`","prompt":"x","native":{"a":1,"a":2}}`), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusBadRequest || len(fixture.snapshot()) != 1 {
			t.Fatalf("ambiguous source dispatched: %d %s", status, raw)
		}
	})
	t.Run("image edit ordered files and mask", func(t *testing.T) {
		result := `{"created":1,"data":[{"url":"https://asset.example/edited"}],"native_number":9007199254740993}`
		fixture := newOperationFixture(t, "/v1/images/edits", result)
		slug, key := publishStrictMediaFixture(t, h, owner, fixture, "image_edit", "unary")
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		writer.WriteField("prompt", "edit")
		for _, item := range []struct{ name, filename, payload string }{{"image[1]", "second.png", "image-second"}, {"mask", "mask.png", "mask-bytes"}} {
			part, err := writer.CreateFormFile(item.name, item.filename)
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte(item.payload))
		}
		writer.WriteField("model", slug)
		part, err := writer.CreateFormFile("image[0]", "first.png")
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte("image-first"))
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/images/edits", key, bytes.NewReader(body.Bytes()), map[string]string{"Content-Type": writer.FormDataContentType()})
		if status != http.StatusOK || string(raw) != result {
			t.Fatalf("native edit status=%d body=%s", status, raw)
		}
		calls := fixture.snapshot()
		if len(calls) != 1 {
			t.Fatalf("edit dispatch count=%d", len(calls))
		}
		_, params, err := mime.ParseMediaType(calls[0].headers.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		reader := multipart.NewReader(bytes.NewReader(calls[0].body), params["boundary"])
		var names, values []string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, part.FormName())
			values = append(values, string(data))
		}
		wantNames := []string{"prompt", "image[1]", "mask", "model", "image[0]"}
		wantValues := []string{"edit", "image-second", "mask-bytes", vendorModel, "image-first"}
		if len(names) != len(wantNames) {
			t.Fatalf("edit members=%v", names)
		}
		for i := range wantNames {
			if names[i] != wantNames[i] || values[i] != wantValues[i] {
				t.Fatalf("edit member %d %q=%q, want %q=%q", i, names[i], values[i], wantNames[i], wantValues[i])
			}
		}
	})
	t.Run("transcription timestamps and file bytes", func(t *testing.T) {
		result := `{"text":"hello","duration":1.0000001,"segments":[{"start":-0,"end":1.0000001,"text":"hello","native_counter":9007199254740993}]}`
		fixture := newOperationFixture(t, "/v1/audio/transcriptions", result)
		slug, key := publishStrictMediaFixture(t, h, owner, fixture, "transcription", "unary")
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		writer.WriteField("model", slug)
		writer.WriteField("response_format", "verbose_json")
		part, err := writer.CreateFormFile("file", "speech.wav")
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte{0, 1, 2, 3})
		writer.Close()
		status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/audio/transcriptions", key, bytes.NewReader(body.Bytes()), map[string]string{"Content-Type": writer.FormDataContentType()})
		if status != http.StatusOK || string(raw) != result {
			t.Fatalf("transcription status=%d body=%s", status, raw)
		}
		calls := fixture.snapshot()
		if len(calls) != 1 || !bytes.Contains(calls[0].body, []byte{0, 1, 2, 3}) {
			t.Fatalf("transcription file lost: %+v", calls)
		}
	})
	t.Run("speech binary", func(t *testing.T) {
		payload := "RIFF\x00\x01\xffWAVE"
		fixture := newOperationFixture(t, "/v1/audio/speech", payload)
		fixture.resultType("audio/wav")
		slug, key := publishStrictMediaFixture(t, h, owner, fixture, "speech", "unary")
		request := `{"model":"` + slug + `","input":"native speech","voice":"alloy","speed":0.500,"native":{"channel":9007199254740993}}`
		status, raw, headers := h.gatewayRaw(http.MethodPost, "/v1/audio/speech", key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || string(raw) != payload || headers.Get("Content-Type") != "audio/wav" {
			t.Fatalf("speech status=%d type=%s body=%x", status, headers.Get("Content-Type"), raw)
		}
		calls := fixture.snapshot()
		want := `{"model":"` + vendorModel + `","input":"native speech","voice":"alloy","speed":0.500,"native":{"channel":9007199254740993}}`
		if len(calls) != 1 || string(calls[0].body) != want {
			t.Fatalf("native speech request=%+v", calls)
		}
	})
	t.Run("image stream events", func(t *testing.T) {
		fixture := newOperationFixture(t, "/v1/images/generations", "id: asset-one\nretry: 2000\nevent: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"native\":{\"sample\":9007199254740993}}\n\nevent: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
		fixture.resultType("text/event-stream")
		slug, key := publishStrictMediaFixture(t, h, owner, fixture, "image_generation", "streaming")
		status, raw, headers := h.gatewayRaw(http.MethodPost, "/v1/images/generations", key, strings.NewReader(`{"model":"`+slug+`","prompt":"frame","stream":true}`), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || !strings.HasPrefix(headers.Get("Content-Type"), "text/event-stream") || !bytes.Contains(raw, []byte("id: asset-one\nretry: 2000")) || !bytes.Contains(raw, []byte("9007199254740993")) || !bytes.Contains(raw, []byte("image_generation.completed")) {
			t.Fatalf("native stream status=%d type=%s body=%s", status, headers.Get("Content-Type"), raw)
		}
		if len(fixture.snapshot()) != 1 {
			t.Fatalf("stream dispatched %d times", len(fixture.snapshot()))
		}
	})
}
