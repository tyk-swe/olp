package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
)

// mediaProvider returns the named fixture provider and its snapshot id.
func mediaProvider(t *testing.T, h *harness, name string) (string, routing.Provider) {
	t.Helper()
	for id, p := range h.rt.release.Snapshot.Providers {
		if p.Name == name {
			return id, p
		}
	}
	t.Fatalf("provider %q not in fixture", name)
	return "", routing.Provider{}
}

// mediaFormBody renders one multipart body with a single file field.
func mediaFormBody(t *testing.T, fields map[string]string, fileField, filename string, file []byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if fileField != "" {
		part, err := w.CreateFormFile(fileField, filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

// mediaRequest issues one media call against the harness server.
func mediaRequest(t *testing.T, h *harness, method, path string, body []byte, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	resp := h.do(t.Context(), method, path, fullKey, body, headers)
	defer resp.Body.Close()
	var decoded map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("invalid JSON response %q: %v", data, err)
		}
	}
	return resp, decoded
}

func imageEditBody(t *testing.T) ([]byte, string) {
	t.Helper()
	return mediaFormBody(t, map[string]string{"model": routeSlug, "prompt": "photo"}, "image[]", "image.png", []byte("png-bytes"))
}

func TestMediaRoutingHeaderValidation(t *testing.T) {
	type endpoint struct {
		body func(t *testing.T) ([]byte, string)
	}
	endpoints := map[string]endpoint{
		"/v1/images/generations": {func(t *testing.T) ([]byte, string) {
			return []byte(`{"model":"team-chat","prompt":"photo"}`), "application/json"
		}},
		"/v1/audio/speech": {func(t *testing.T) ([]byte, string) {
			return []byte(`{"model":"team-chat","input":"hello","voice":"alloy"}`), "application/json"
		}},
		"/v1/images/edits": {func(t *testing.T) ([]byte, string) { return imageEditBody(t) }},
		"/v1/audio/transcriptions": {func(t *testing.T) ([]byte, string) {
			return mediaFormBody(t, map[string]string{"model": routeSlug}, "file", "audio.mp3", []byte("audio-bytes"))
		}},
		"/v1/videos": {func(t *testing.T) ([]byte, string) {
			return mediaFormBody(t, map[string]string{"model": routeSlug, "prompt": "clip"}, "", "", nil)
		}},
	}
	for path, ep := range endpoints {
		t.Run(path, func(t *testing.T) {
			h := newMediaHarness(t)
			body, contentType := ep.body(t)
			for _, header := range []string{
				`{"max_attempts":999}`, // cannot widen the published budget
				`{"strategy":"unknown"}`,
				`{"unknown":1}`,
				`not json`,
			} {
				resp, decoded := mediaRequest(t, h, http.MethodPost, path, body,
					map[string]string{"Content-Type": contentType, routingHeader: header})
				if resp.StatusCode != http.StatusBadRequest || errorCode(t, decoded) != "invalid_request" {
					t.Fatalf("header %q: status=%d body=%v", header, resp.StatusCode, decoded)
				}
			}
			if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
				t.Fatalf("rejected routing header reached upstream %d times", calls)
			}
		})
	}
}

func TestMediaRoutingPreferencesNarrowAndExclude(t *testing.T) {
	t.Run("budget narrowed on JSON", func(t *testing.T) {
		h := newMediaHarness(t)
		h.mock.set("a", status(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`))
		resp, decoded := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo"}`),
			map[string]string{routingHeader: `{"max_attempts":1}`})
		if resp.StatusCode != http.StatusBadGateway || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
			t.Fatalf("narrowed budget: status=%d calls a=%d b=%d body=%v", resp.StatusCode, h.mock.count("a"), h.mock.count("b"), decoded)
		}
	})
	t.Run("budget narrowed on multipart", func(t *testing.T) {
		h := newMediaHarness(t)
		h.mock.set("a", status(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`))
		body, contentType := imageEditBody(t)
		resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/edits", body,
			map[string]string{"Content-Type": contentType, routingHeader: `{"max_attempts":1}`})
		if resp.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
			t.Fatalf("narrowed budget: status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
	})
	t.Run("provider exclusion on JSON", func(t *testing.T) {
		h := newMediaHarness(t)
		aID, _ := mediaProvider(t, h, "a")
		resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo"}`),
			map[string]string{routingHeader: `{"ignore":["provider:` + aID + `"]}`})
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
			t.Fatalf("exclusion: status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
	})
	t.Run("provider exclusion on multipart", func(t *testing.T) {
		h := newMediaHarness(t)
		aID, _ := mediaProvider(t, h, "a")
		body, contentType := imageEditBody(t)
		resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/edits", body,
			map[string]string{"Content-Type": contentType, routingHeader: `{"ignore":["provider:` + aID + `"]}`})
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
			t.Fatalf("exclusion: status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
	})
	t.Run("privacy restriction on JSON", func(t *testing.T) {
		h := newMediaHarness(t)
		aID, a := mediaProvider(t, h, "a")
		bID, b := mediaProvider(t, h, "b")
		a.Models = map[string]json.RawMessage{modelA: json.RawMessage(`{"data_collection":true,"source":"catalog","observed_at":"2025-01-01T00:00:00Z"}`)}
		b.Models = map[string]json.RawMessage{modelB: json.RawMessage(`{"data_collection":false,"source":"catalog","observed_at":"2025-01-01T00:00:00Z"}`)}
		h.rt.release.Snapshot.Providers[aID] = a
		h.rt.release.Snapshot.Providers[bID] = b
		resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo"}`),
			map[string]string{routingHeader: `{"deny_data_collection":true}`})
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
			t.Fatalf("privacy: status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
	})
	t.Run("privacy restriction on multipart", func(t *testing.T) {
		h := newMediaHarness(t)
		aID, a := mediaProvider(t, h, "a")
		bID, b := mediaProvider(t, h, "b")
		a.Models = map[string]json.RawMessage{modelA: json.RawMessage(`{"zero_data_retention":false,"source":"catalog","observed_at":"2025-01-01T00:00:00Z"}`)}
		b.Models = map[string]json.RawMessage{modelB: json.RawMessage(`{"zero_data_retention":true,"source":"catalog","observed_at":"2025-01-01T00:00:00Z"}`)}
		h.rt.release.Snapshot.Providers[aID] = a
		h.rt.release.Snapshot.Providers[bID] = b
		body, contentType := imageEditBody(t)
		resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/edits", body,
			map[string]string{"Content-Type": contentType, routingHeader: `{"require_zero_data_retention":true}`})
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
			t.Fatalf("privacy: status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
	})
}

func TestMediaStrictParameterFiltering(t *testing.T) {
	observed := `"source":"catalog","observed_at":"2025-01-01T00:00:00Z","data_collection":false,"zero_data_retention":true`
	setSupported := func(h *harness, name string, params string) {
		id, p := mediaProvider(t, h, name)
		p.Models = map[string]json.RawMessage{
			modelA: json.RawMessage(`{"supported_parameters":[` + params + `],` + observed + `}`),
			modelB: json.RawMessage(`{"supported_parameters":[` + params + `],` + observed + `}`),
		}
		h.rt.release.Snapshot.Providers[id] = p
	}
	setProfile := func(h *harness, defaults map[string]json.RawMessage) {
		profile, err := connectors.LookupProfile("compatible-chat", "1")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a", "b"} {
			id, p := mediaProvider(t, h, name)
			p.ProfileID, p.ProfileRevision = profile.ID, profile.Revision
			p.OperationDefaults = map[string]connectors.DefaultSet{media.OpImageGeneration: {
				Dialect: profile.OperationDialect(media.OpImageGeneration), Values: defaults,
			}}
			h.rt.release.Snapshot.Providers[id] = p
		}
	}
	t.Run("explicit native null is a supplied parameter", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		setProfile(h, nil)
		resp, decoded := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo","n":null}`),
			map[string]string{routingHeader: `{"require_parameters":true}`})
		if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, decoded) != "upstream_unavailable" || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("native null escaped parameter filter: status=%d calls=%d body=%v", resp.StatusCode, h.mock.count("a")+h.mock.count("b"), decoded)
		}
	})
	t.Run("profile default is an effective parameter", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		setProfile(h, map[string]json.RawMessage{"quality": json.RawMessage(`"high"`)})
		resp, decoded := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo"}`),
			map[string]string{routingHeader: `{"require_parameters":true}`})
		if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, decoded) != "upstream_unavailable" || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("default escaped parameter filter: status=%d calls=%d body=%v", resp.StatusCode, h.mock.count("a")+h.mock.count("b"), decoded)
		}
	})
	t.Run("staged file bytes are not a metadata parameter", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		setProfile(h, nil)
		h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"created":1,"data":[{"b64_json":"iVBORw=="}]}`)
		})
		body, contentType := imageEditBody(t)
		resp, decoded := mediaRequest(t, h, http.MethodPost, "/v1/images/edits", body,
			map[string]string{"Content-Type": contentType, routingHeader: `{"require_parameters":true}`})
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
			t.Fatalf("staged image became metadata parameter: status=%d calls=%d body=%v", resp.StatusCode, h.mock.count("a"), decoded)
		}
	})
	t.Run("unsupported supplied control rejects before dispatch", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		resp, decoded := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo","size":"1024x1024"}`),
			map[string]string{routingHeader: `{"require_parameters":true}`})
		if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, decoded) != "upstream_unavailable" {
			t.Fatalf("strict filter: status=%d body=%v", resp.StatusCode, decoded)
		}
		if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
			t.Fatalf("unsupported control dispatched %d times", calls)
		}
	})
	t.Run("unknown supplied control rejects before dispatch", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		resp, decoded := mediaRequest(t, h, http.MethodPost, "/v1/audio/speech",
			[]byte(`{"model":"team-chat","input":"hi","voice":"alloy","vendor_dial":"east"}`),
			map[string]string{routingHeader: `{"require_parameters":true}`})
		if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, decoded) != "upstream_unavailable" {
			t.Fatalf("strict filter: status=%d body=%v", resp.StatusCode, decoded)
		}
		if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
			t.Fatalf("unknown control dispatched %d times", calls)
		}
	})
	t.Run("transport and delivery fields are not requirements", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
		})
		// stream is a delivery field and model the route slug: neither may be
		// demanded of the upstream's supported parameter list.
		resp := h.do(t.Context(), http.MethodPost, "/v1/images/generations", fullKey,
			[]byte(`{"model":"team-chat","prompt":"photo","stream":true}`),
			map[string]string{routingHeader: `{"require_parameters":true}`})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
			t.Fatalf("delivery fields leaked into requirements: status=%d calls=%d", resp.StatusCode, h.mock.count("a"))
		}
	})
	t.Run("non-strict filtering unchanged", func(t *testing.T) {
		h := newMediaHarness(t)
		setSupported(h, "a", `"prompt"`)
		setSupported(h, "b", `"prompt"`)
		resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
			[]byte(`{"model":"team-chat","prompt":"photo","size":"1024x1024","vendor_dial":"east"}`), nil)
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
			t.Fatalf("non-strict request: status=%d calls=%d", resp.StatusCode, h.mock.count("a"))
		}
	})
}

// a hard quota with no limiter configured is unmeterable

// While the half-open probe is in flight the provider rejects every
// sibling dispatch, so a concurrent request must use the next target.

// An image edit is not idempotent: a post-dispatch server failure leaves
// the upstream outcome unprovable, so no sibling may retry it.

// The first credential's call is pending when authority revokes its sibling.
