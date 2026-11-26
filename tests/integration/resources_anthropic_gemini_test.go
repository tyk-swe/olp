//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
)

func anthropicReplies(string, int) map[string]string {
	const file = `{"id":"file_provider-owned","type":"file","filename":"a.txt","mime_type":"text/plain","size_bytes":5,"created_at":"2026-01-01T00:00:00Z","downloadable":true}`
	batch := func(status string) string {
		return `{"id":"msgbatch_provider-owned","type":"message_batch","processing_status":"` + status + `","request_counts":{"processing":1,"succeeded":0,"errored":0,"canceled":0,"expired":0},"created_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-02T00:00:00Z","results_url":null}`
	}
	return map[string]string{
		"POST /files":                                           file,
		"GET /files/file_provider-owned":                        file,
		"GET /files/file_provider-owned/content":                "CONTENT",
		"DELETE /files/file_provider-owned":                     `{"id":"file_provider-owned","type":"file_deleted"}`,
		"POST /messages/batches":                                batch("in_progress"),
		"GET /messages/batches/msgbatch_provider-owned":         batch("in_progress"),
		"POST /messages/batches/msgbatch_provider-owned/cancel": batch("canceling"),
		"GET /messages/batches/msgbatch_provider-owned/results": `{"custom_id":"one","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}}}` + "\n",
	}
}

func TestAnthropicFilesAndBatchesHideProviderIdentifiersIsolateKeysAndKeepUnsettledBatches(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "claude-route", "claude-model"
	upstream := &retainedUpstream{replies: anthropicReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "anthropic", surface: "anthropic", operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const files, batches = "/anthropic/v1/files", "/anthropic/v1/messages/batches"

	upload := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	file := g.call(t, "owner", "POST", files, "multipart/form-data; boundary=b", upload, 200)
	if file.ID == "" {
		t.Fatal("upload returned no file identifier")
	}
	uploaded := upstream.sent("POST /files")
	if len(uploaded) != 1 || uploaded[0].key != "listed-secret" || uploaded[0].authorization != "" || !strings.Contains(uploaded[0].body, "hello") {
		t.Fatalf("upload reached the provider as %+v, want the file under the provider credential only", uploaded)
	}
	if got := g.call(t, "owner", "GET", files+"/"+file.ID, "", "", 200); got.ID != file.ID {
		t.Fatalf("file read returned %q, want %q", got.ID, file.ID)
	}
	if got := g.call(t, "owner", "GET", files, "", "", 200).identifiers(); !slices.Equal(got, []string{file.ID}) {
		t.Fatalf("owner lists files %v, want %v", got, []string{file.ID})
	}
	g.call(t, "owner", "GET", files+"/"+file.ID+"/content", "", "", 200)

	submission := `{"requests":[{"custom_id":"one","params":{"model":"` + slug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}}]}`
	batch := g.call(t, "owner", "POST", batches, "application/json", submission, 200)
	if batch.ID == "" {
		t.Fatal("batch creation returned no identifier")
	}
	submitted := upstream.sent("POST /messages/batches")
	if len(submitted) != 1 || !strings.Contains(submitted[0].body, `"model":"`+model+`"`) || strings.Contains(submitted[0].body, slug) {
		t.Fatalf("batch reached the provider as %+v, want the provider model in place of the route", submitted)
	}
	if got := g.call(t, "owner", "GET", batches+"/"+batch.ID, "", "", 200); got.ID != batch.ID {
		t.Fatalf("batch read returned %q, want %q", got.ID, batch.ID)
	}
	if got := g.call(t, "owner", "GET", batches, "", "", 200).identifiers(); !slices.Equal(got, []string{batch.ID}) {
		t.Fatalf("owner lists batches %v, want %v", got, []string{batch.ID})
	}

	if got := g.call(t, "stranger", "GET", files, "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists files %v", got)
	}
	if got := g.call(t, "stranger", "GET", batches, "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists batches %v", got)
	}
	g.call(t, "stranger", "GET", files+"/"+file.ID, "", "", 404)
	g.call(t, "stranger", "GET", files+"/"+file.ID+"/content", "", "", 404)
	g.call(t, "stranger", "DELETE", files+"/"+file.ID, "", "", 404)
	g.call(t, "stranger", "GET", batches+"/"+batch.ID, "", "", 404)
	g.call(t, "stranger", "GET", batches+"/"+batch.ID+"/results", "", "", 404)
	g.call(t, "stranger", "POST", batches+"/"+batch.ID+"/cancel", "", "", 404)
	g.call(t, "stranger", "DELETE", batches+"/"+batch.ID, "", "", 404)

	if got := g.call(t, "owner", "POST", batches+"/"+batch.ID+"/cancel", "", "", 200); got.ID != batch.ID || got.ProcessingStatus != "canceling" {
		t.Fatalf("batch cancel = %+v", got)
	}
	g.call(t, "owner", "GET", batches+"/"+batch.ID+"/results", "", "", 200)
	g.call(t, "owner", "DELETE", batches+"/"+batch.ID, "", "", 409)
	if got := g.call(t, "owner", "GET", batches+"/"+batch.ID, "", "", 200); got.ID != batch.ID {
		t.Fatalf("an unsettled batch disappeared after a refused delete: %+v", got)
	}
	if got := g.call(t, "owner", "DELETE", files+"/"+file.ID, "", "", 200); got.ID != file.ID {
		t.Fatalf("file delete = %+v", got)
	}
	g.call(t, "owner", "GET", files+"/"+file.ID, "", "", 404)
	for request, want := range map[string]int{
		"POST /messages/batches/msgbatch_provider-owned/cancel": 1,
		"GET /messages/batches/msgbatch_provider-owned/results": 1,
		"DELETE /messages/batches/msgbatch_provider-owned":      0,
		"DELETE /files/file_provider-owned":                     1,
	} {
		if sent := upstream.sent(request); len(sent) != want {
			t.Fatalf("the provider saw %d of %q, want %d", len(sent), request, want)
		}
	}
	fileBeta := uploaded[0].beta
	if fileBeta == "" {
		t.Fatal("the upload did not opt into the provider's file API")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for _, sent := range upstream.calls {
		onFile := strings.Contains(sent.request, " /files")
		if onFile && sent.beta != fileBeta || !onFile && sent.beta != "" {
			t.Fatalf("%q reached the provider with beta %q, want the file API opt-in on file requests only", sent.request, sent.beta)
		}
		download := strings.HasSuffix(sent.request, "/content")
		if binary := sent.accept == "application/binary"; binary != download {
			t.Fatalf("%q asked the provider for %q, want binary for a file download only", sent.request, sent.accept)
		}
	}
}

func geminiReplies(_ string, nth int) map[string]string {
	now := time.Now().UTC()
	file := fmt.Sprintf(`{"name":"files/%[1]s","displayName":"notes","mimeType":"text/plain","sizeBytes":"5","createTime":%[2]q,"updateTime":%[2]q,"expirationTime":%[3]q,"sha256Hash":"aGFzaA==","uri":"https://provider.invalid/v1beta/files/%[1]s","state":"ACTIVE"}`,
		providerOwned, now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	interaction := func(n int) string {
		return fmt.Sprintf(`{"id":"interaction-%s-%d","status":"completed","model":"gemini-model","steps":[{"type":"model_output","content":[{"type":"text","text":"hello"}]}],"usage":{"total_input_tokens":3,"total_output_tokens":2,"total_tokens":5}}`, providerOwned, n)
	}
	return map[string]string{
		"POST /upload/v1beta/files":                                "",
		"POST /upload/session-" + providerOwned:                    `{"file":` + file + `}`,
		"GET /files/" + providerOwned:                              file,
		"DELETE /files/" + providerOwned:                           `{}`,
		"POST /interactions":                                       interaction(nth),
		"GET /interactions/interaction-" + providerOwned + "-1":    interaction(1),
		"DELETE /interactions/interaction-" + providerOwned + "-1": `{}`,
	}
}

func geminiUploadHeaders(r *http.Request) map[string]string {
	switch r.Header.Get("X-Goog-Upload-Command") {
	case "start":
		return map[string]string{"X-Goog-Upload-URL": "https://" + r.Host + "/upload/session-" + providerOwned, "X-Goog-Upload-Status": "active"}
	case "upload, finalize":
		return map[string]string{"X-Goog-Upload-Status": "final"}
	}
	return nil
}

func geminiKey(key string, extra map[string]string) map[string]string {
	headers := maps.Clone(extra)
	if headers == nil {
		headers = map[string]string{}
	}
	headers["x-goog-api-key"] = key
	return headers
}

type geminiFile struct {
	Name, URI string
}

func TestGeminiFilesUploadThroughAGatewaySessionHideProviderIdentifiersAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	upstream := &retainedUpstream{replies: geminiReplies, headers: geminiUploadHeaders}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "gemini-files", model: "gemini-model", kind: "gemini", surface: "gemini", version: "/v1beta", secure: true,
		operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const files = "/gemini/v1beta/"

	started, _ := g.send(t, "POST", "/gemini/upload/v1beta/files", geminiKey("owner", map[string]string{
		"Content-Type": "application/json", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
		"X-Goog-Upload-Header-Content-Length": "5", "X-Goog-Upload-Header-Content-Type": "text/plain",
	}), `{"file":{"display_name":"notes"}}`, 200)
	session, found := strings.CutPrefix(started.Get("X-Goog-Upload-URL"), g.server.URL)
	if !found || !strings.HasPrefix(session, "/gemini/upload/") {
		t.Fatalf("upload session is %q, want a session on the gateway", started.Get("X-Goog-Upload-URL"))
	}
	finalize := map[string]string{"X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"}
	g.send(t, "POST", session, geminiKey("stranger", finalize), "stolen", 404)
	if sent := upstream.sent("POST /upload/session-" + providerOwned); len(sent) != 0 {
		t.Fatalf("another key's bytes reached the provider session: %+v", sent)
	}
	_, raw := g.send(t, "POST", session, geminiKey("owner", finalize), "hello", 200)
	var uploaded struct{ File geminiFile }
	if err := json.Unmarshal(raw, &uploaded); err != nil || !strings.HasPrefix(uploaded.File.Name, "files/") || uploaded.File.URI != g.server.URL+files+uploaded.File.Name {
		t.Fatalf("finalized upload = %s, want a file named and addressed by the gateway", raw)
	}
	name := uploaded.File.Name
	sent := upstream.sent("POST /upload/session-" + providerOwned)
	if len(sent) != 1 || sent[0].key != "listed-secret" || sent[0].body != "hello" {
		t.Fatalf("the provider session received %+v, want the owner's bytes under the provider credential", sent)
	}

	listed := func(key string) []string {
		_, raw := g.send(t, "GET", files+"files", geminiKey(key, nil), "", 200)
		var page struct{ Files []geminiFile }
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(page.Files))
		for i, file := range page.Files {
			names[i] = file.Name
		}
		return names
	}
	_, raw = g.send(t, "GET", files+name, geminiKey("owner", nil), "", 200)
	var read geminiFile
	if err := json.Unmarshal(raw, &read); err != nil || read.Name != name {
		t.Fatalf("file read = %s, want %q", raw, name)
	}
	if got := listed("owner"); !slices.Equal(got, []string{name}) {
		t.Fatalf("owner lists files %v, want %v", got, []string{name})
	}
	if got := listed("stranger"); len(got) != 0 {
		t.Fatalf("another key lists files %v", got)
	}
	g.send(t, "GET", files+name, geminiKey("stranger", nil), "", 404)
	g.send(t, "DELETE", files+name, geminiKey("stranger", nil), "", 404)

	g.send(t, "DELETE", files+name, geminiKey("owner", nil), "", 200)
	g.send(t, "GET", files+name, geminiKey("owner", nil), "", 404)
	if got := listed("owner"); len(got) != 0 {
		t.Fatalf("a deleted file is still listed: %v", got)
	}
	if sent := upstream.sent("DELETE /files/" + providerOwned); len(sent) != 1 || sent[0].key != "listed-secret" {
		t.Fatalf("the provider saw deletes %+v, want one under the provider credential", sent)
	}
}

func TestGeminiUploadsResumeInChunksFromTheStoredOffsetAndFailTheSessionOnAProviderMismatch(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	upstream := &retainedUpstream{replies: geminiReplies, headers: func(r *http.Request) map[string]string {
		if r.Header.Get("X-Goog-Upload-Command") == "upload" && r.ContentLength == 3 {
			return map[string]string{"X-Goog-Upload-Size-Received": "9"}
		}
		return geminiUploadHeaders(r)
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "gemini-chunks", model: "gemini-model", kind: "gemini", surface: "gemini", version: "/v1beta", secure: true,
		operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	start := func() string {
		t.Helper()
		started, _ := g.send(t, "POST", "/gemini/upload/v1beta/files", geminiKey("owner", map[string]string{
			"Content-Type": "application/json", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
			"X-Goog-Upload-Header-Content-Length": "10", "X-Goog-Upload-Header-Content-Type": "text/plain",
		}), `{"file":{"display_name":"notes"}}`, 200)
		session, _ := strings.CutPrefix(started.Get("X-Goog-Upload-URL"), g.server.URL)
		return session
	}
	command := func(session, command, offset, body string, want int) http.Header {
		t.Helper()
		reply, _ := g.send(t, "POST", session, geminiKey("owner", map[string]string{"X-Goog-Upload-Command": command, "X-Goog-Upload-Offset": offset}), body, want)
		return reply
	}
	received := func(session, want, status string) {
		t.Helper()
		reply := command(session, "query", "", "", 200)
		if got := reply.Get("X-Goog-Upload-Size-Received"); got != want || reply.Get("X-Goog-Upload-Status") != status {
			t.Fatalf("session reports %q bytes with status %q, want %q and %q", got, reply.Get("X-Goog-Upload-Status"), want, status)
		}
	}

	session := start()
	received(session, "0", "active")
	command(session, "append", "0", "hello", 400)
	command(session, "query", "", "hello", 400)
	if reply := command(session, "upload", "0", "hello", 200); reply.Get("X-Goog-Upload-Size-Received") != "5" || reply.Get("X-Goog-Upload-Status") != "active" {
		t.Fatalf("first chunk acknowledged as %v, want five bytes on an active session", reply)
	}
	received(session, "5", "active")
	command(session, "upload", "0", "hello", 400)
	command(session, "upload", "5", "far too long", 400)
	command(session, "upload, finalize", "5", "wor", 400)
	received(session, "5", "active")
	if reply := command(session, "upload, finalize", "5", "world", 200); reply.Get("X-Goog-Upload-Size-Received") != "10" || reply.Get("X-Goog-Upload-Status") != "final" {
		t.Fatalf("last chunk acknowledged as %v, want ten bytes on a final session", reply)
	}
	sent := upstream.sent("POST /upload/session-" + providerOwned)
	if len(sent) != 2 || sent[0].body != "hello" || sent[0].upload != "upload@0" || sent[1].body != "world" || sent[1].upload != "upload, finalize@5" {
		t.Fatalf("the provider session received %+v, want the two accepted chunks at offsets 0 and 5", sent)
	}

	broken := start()
	command(broken, "upload", "0", "abc", 502)
	command(broken, "query", "", "", 409)
	command(broken, "upload", "0", "hello", 409)
}

func TestGeminiInteractionsHideProviderIdentifiersContinueOnTheirProviderAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "gemini-interactions", "gemini-model"
	upstream := &retainedUpstream{replies: geminiReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true,
		operations: []string{"generation"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const interactions = "/gemini/v1beta/interactions"
	type interaction struct{ ID, Model, Status string }
	create := func(key, body string, want int) interaction {
		t.Helper()
		_, raw := g.send(t, "POST", interactions, geminiKey(key, map[string]string{"Content-Type": "application/json"}), body, want)
		var decoded interaction
		_ = json.Unmarshal(raw, &decoded)
		return decoded
	}

	first := create("owner", `{"model":"`+slug+`","input":"hi"}`, 200)
	if first.ID == "" || first.Model != slug || first.Status != "completed" {
		t.Fatalf("interaction = %+v, want a gateway identifier under the route name", first)
	}
	created := upstream.sent("POST /interactions")
	if len(created) != 1 || created[0].key != "listed-secret" || !strings.Contains(created[0].body, `"model":"`+model+`"`) || strings.Contains(created[0].body, slug) {
		t.Fatalf("interaction reached the provider as %+v, want the provider model under the provider credential", created)
	}
	_, raw := g.send(t, "GET", interactions+"/"+first.ID, geminiKey("owner", nil), "", 200)
	var read interaction
	if err := json.Unmarshal(raw, &read); err != nil || read.ID != first.ID || read.Model != slug {
		t.Fatalf("interaction read = %s, want %q under the route name", raw, first.ID)
	}

	g.send(t, "GET", interactions+"/"+first.ID, geminiKey("stranger", nil), "", 404)
	g.send(t, "DELETE", interactions+"/"+first.ID, geminiKey("stranger", nil), "", 404)
	create("stranger", `{"model":"`+slug+`","input":"again","previous_interaction_id":"`+first.ID+`"}`, 400)
	create("stranger", `{"model":"`+slug+`","input":"again","previous_interaction_id":"interaction_0123456789abcdef0123456789abcdef"}`, 400)
	if sent := upstream.sent("POST /interactions"); len(sent) != 1 {
		t.Fatalf("another key's continuation reached the provider: %+v", sent)
	}

	second := create("owner", `{"model":"`+slug+`","input":"again","previous_interaction_id":"`+first.ID+`"}`, 200)
	if second.ID == "" || second.ID == first.ID {
		t.Fatalf("continuation = %+v, want its own identifier", second)
	}
	continued := upstream.sent("POST /interactions")
	if len(continued) != 2 || !strings.Contains(continued[1].body, `"previous_interaction_id":"interaction-`+providerOwned+`-1"`) || strings.Contains(continued[1].body, first.ID) {
		t.Fatalf("continuation reached the provider as %+v, want the provider's parent identifier", continued)
	}

	g.send(t, "DELETE", interactions+"/"+first.ID, geminiKey("owner", nil), "", 200)
	g.send(t, "GET", interactions+"/"+first.ID, geminiKey("owner", nil), "", 404)
	if sent := upstream.sent("DELETE /interactions/interaction-" + providerOwned + "-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want 1", len(sent))
	}
}

func interactionEvent(id int, body string) string {
	return fmt.Sprintf("id: %d\ndata: %s\n\n", id, body)
}

func interactionStreamReplies(request string, nth int) map[string]string {
	native := "interaction-" + providerOwned + "-1"
	lifecycle := func(event, id, status string) string {
		return `{"event_type":"` + event + `","interaction":{"id":"` + id + `","status":"` + status + `","model":"gemini-model","usage":{"total_input_tokens":3,"total_output_tokens":2,"total_tokens":5}}}`
	}
	start := interactionEvent(2, `{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`)
	rest := interactionEvent(3, `{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"hello"}}`) +
		interactionEvent(4, `{"event_type":"step.stop","index":0}`)
	created := map[int]string{
		1: interactionEvent(1, lifecycle("interaction.created", native, "in_progress")) + start + rest + interactionEvent(5, lifecycle("interaction.completed", native, "completed")),
		2: start + rest,
	}
	resumed := map[int]string{
		1: rest + interactionEvent(5, lifecycle("interaction.completed", native, "completed")),
		2: rest + interactionEvent(5, lifecycle("interaction.completed", "interaction-"+providerOwned+"-2", "completed")),
	}
	return map[string]string{
		"POST /interactions":                       created[nth],
		"GET /interactions/" + native:              resumed[nth],
		"POST /interactions/" + native + "/cancel": `{"id":"` + native + `","status":"cancelled","model":"gemini-model","steps":[]}`,
	}
}

func TestNewGeminiInteractionsHoldTheirReplyBoundAgainstOutputTokenLimits(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "gemini-interactions", "gemini-model"
	upstream := &retainedUpstream{replies: geminiReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true,
		operations: []string{"generation"}, upstream: upstream, limits: &routing.Limits{OutputTokensPerMinute: new(int64(100))},
	})
	g.key(t, "owner")
	const interactions = "/gemini/v1beta/interactions"
	headers := geminiKey("owner", map[string]string{"Content-Type": "application/json"})
	create := func(replyBound string) string {
		return `{"model":"` + slug + `","input":"hi","generation_config":{"max_output_tokens":` + replyBound + `}}`
	}

	g.send(t, "POST", interactions, headers, create("500"), 429)
	if sent := upstream.sent("POST /interactions"); len(sent) != 0 {
		t.Fatalf("an Interaction allowed 500 output tokens reached the provider under a 100 token output limit: %+v", sent)
	}
	g.send(t, "POST", interactions, headers, create("50"), 200)
}

type streamedInteraction struct {
	ids    []string
	events []struct {
		Type        string `json:"event_type"`
		Interaction struct{ ID, Model, Status string }
	}
}

func readInteractionStream(t *testing.T, header http.Header, raw []byte) streamedInteraction {
	t.Helper()
	if kind := header.Get("Content-Type"); kind != "text/event-stream" {
		t.Fatalf("content type = %q, want an event stream: %s", kind, raw)
	}
	var stream streamedInteraction
	for line := range strings.SplitSeq(string(raw), "\n") {
		if id, found := strings.CutPrefix(line, "id: "); found {
			stream.ids = append(stream.ids, id)
		}
		data, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		stream.events = append(stream.events, struct {
			Type        string `json:"event_type"`
			Interaction struct{ ID, Model, Status string }
		}{})
		if err := json.Unmarshal([]byte(data), &stream.events[len(stream.events)-1]); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
	}
	return stream
}

func TestGeminiInteractionStreamsBindTheirResourceResumeFromACursorCancelAndRefuseBrokenStreams(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, native = "gemini-streams", "interaction-" + providerOwned + "-1"
	upstream := &retainedUpstream{replies: interactionStreamReplies, headers: func(r *http.Request) map[string]string {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/interactions") || r.URL.Query().Get("stream") == "true" {
			return map[string]string{"Content-Type": "text/event-stream"}
		}
		return nil
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "gemini-model", kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true, streams: true,
		operations: []string{"generation"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const interactions = "/gemini/v1beta/interactions"
	body := `{"model":"` + slug + `","input":"hi","stream":true,"store":true}`
	json := map[string]string{"Content-Type": "application/json"}

	header, raw := g.send(t, "POST", interactions, geminiKey("owner", json), body, 200)
	created := readInteractionStream(t, header, raw)
	if len(created.events) != 5 || !slices.Equal(created.ids, []string{"1", "2", "3", "4", "5"}) {
		t.Fatalf("stream = %s, want the provider's five events under their event identifiers", raw)
	}
	first, last := created.events[0], created.events[4]
	if !strings.HasPrefix(first.Interaction.ID, "interaction_") || last.Interaction.ID != first.Interaction.ID ||
		first.Interaction.Model != slug || last.Type != "interaction.completed" || last.Interaction.Status != "completed" {
		t.Fatalf("stream = %s, want one gateway identifier under the route name through to completion", raw)
	}
	local := interactions + "/" + first.Interaction.ID

	for _, refused := range []struct{ method, path string }{
		{"GET", local + "?last_event_id=3"},
		{"GET", local + "?stream=maybe"},
		{"GET", local + "?stream=true&last_event_id=" + strings.Repeat("9", 513)},
		{"POST", local + "/cancel?stream=true"},
		{"DELETE", local + "?include_input=true"},
	} {
		g.send(t, refused.method, refused.path, geminiKey("owner", nil), "", 400)
	}
	g.send(t, "GET", local+"?stream=true&last_event_id=3", geminiKey("stranger", nil), "", 404)
	g.send(t, "POST", local+"/cancel", geminiKey("stranger", nil), "", 404)
	if sent := upstream.sent("GET /interactions/" + native); len(sent) != 0 {
		t.Fatalf("a refused retrieval reached the provider: %+v", sent)
	}

	header, raw = g.send(t, "GET", local+"?stream=true&last_event_id=3&include_input=false", geminiKey("owner", nil), "", 200)
	resumed := readInteractionStream(t, header, raw)
	if len(resumed.events) != 3 || !slices.Equal(resumed.ids, []string{"3", "4", "5"}) || resumed.events[2].Interaction.ID != first.Interaction.ID || resumed.events[2].Interaction.Model != slug {
		t.Fatalf("resumed stream = %s, want the remaining events for %s", raw, first.Interaction.ID)
	}
	if sent := upstream.sent("GET /interactions/" + native); len(sent) != 1 || sent[0].query != "include_input=false&last_event_id=3&stream=true" || sent[0].key != "listed-secret" {
		t.Fatalf("resume reached the provider as %+v, want the cursor on the provider's interaction", sent)
	}
	header, raw = g.send(t, "GET", local+"?stream=true&last_event_id=3", geminiKey("owner", nil), "", 200)
	if cut := readInteractionStream(t, header, raw); !slices.Equal(cut.ids, []string{"3", "4"}) {
		t.Fatalf("stream = %s, want it cut before the event that names another interaction", raw)
	}

	_, raw = g.send(t, "POST", local+"/cancel", geminiKey("owner", nil), "", 200)
	if !strings.Contains(string(raw), `"id":"`+first.Interaction.ID+`"`) || !strings.Contains(string(raw), `"status":"cancelled"`) || !strings.Contains(string(raw), `"model":"`+slug+`"`) {
		t.Fatalf("cancel = %s, want the cancelled interaction under its gateway identifier", raw)
	}
	if sent := upstream.sent("POST /interactions/" + native + "/cancel"); len(sent) != 1 || sent[0].body != "" {
		t.Fatalf("cancel reached the provider as %+v, want one bodiless call", sent)
	}

	g.send(t, "POST", interactions, geminiKey("owner", json), body, 502)
	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources`).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored interactions = %d, %v, want only the one whose stream opened correctly", stored, err)
	}
}

func TestFailedGeminiInteractionsReachTheCallerWithoutTheProviderCredentialOrARetryHint(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, native = "gemini-failures", "interaction-" + providerOwned + "-1"
	failure := `"status":"failed","model":"gemini-model","error":{"code":"internal","message":"credential listed-secret was rejected"}`
	upstream := &retainedUpstream{
		replies: func(string, int) map[string]string {
			return map[string]string{"POST /interactions": `{"id":"` + native + `",` + failure + `,"steps":[]}`}
		},
		headers: func(*http.Request) map[string]string { return nil },
	}
	streamed := &retainedUpstream{
		replies: func(string, int) map[string]string {
			return map[string]string{"POST /interactions": interactionEvent(1, `{"event_type":"interaction.created","interaction":{"id":"`+native+`","status":"in_progress","model":"gemini-model"}}`) +
				interactionEvent(2, `{"event_type":"interaction.completed","interaction":{"id":"`+native+`",`+failure+`}}`)}
		},
		headers: func(*http.Request) map[string]string { return map[string]string{"Content-Type": "text/event-stream"} },
	}
	plan := storedPlan{
		model: "gemini-model", kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true, streams: true,
		operations: []string{"generation"},
	}
	headers := geminiKey("owner", map[string]string{"Content-Type": "application/json"})

	plan.slug, plan.upstream = slug, upstream
	whole := newStoredGateway(t, newUsageFixture(t, pool), plan)
	whole.key(t, "owner")
	header, raw := whole.send(t, "POST", "/gemini/v1beta/interactions", headers, `{"model":"`+slug+`","input":"hi"}`, 200)
	var failed struct{ ID, Model, Status string }
	if err := json.Unmarshal(raw, &failed); err != nil || failed.Status != "failed" || failed.Model != slug || failed.ID == "" {
		t.Fatalf("failed interaction = %s, want the provider's failure under the route name", raw)
	}
	if strings.Contains(string(raw), "listed-secret") || header.Get("X-Should-Retry") != "false" {
		t.Fatalf("failed interaction = %s with retry hint %q, want no credential and no retry", raw, header.Get("X-Should-Retry"))
	}

	plan.slug, plan.upstream = slug+"-stream", streamed
	live := newStoredGateway(t, newUsageFixture(t, pool), plan)
	live.key(t, "owner")
	header, raw = live.send(t, "POST", "/gemini/v1beta/interactions", headers, `{"model":"`+slug+`-stream","input":"hi","stream":true}`, 200)
	stream := readInteractionStream(t, header, raw)
	if len(stream.events) != 2 || stream.events[1].Interaction.Status != "failed" || stream.events[1].Interaction.Model != slug+"-stream" {
		t.Fatalf("failed stream = %s, want the provider's terminal failure under the route name", raw)
	}
	if strings.Contains(string(raw), "listed-secret") || header.Get("X-Should-Retry") != "false" {
		t.Fatalf("failed stream = %s with retry hint %q, want no credential and no retry", raw, header.Get("X-Should-Retry"))
	}
}

func geminiBatchReplies(request string, nth int) map[string]string {
	const native = "batches/batch-" + providerOwned
	batch := func(state, extra string) string {
		return `{"name":"` + native + `","metadata":{"state":"` + state + `","model":"models/gemini-model","displayName":"nightly","createTime":"2026-01-01T00:00:00Z","batchStats":{"requestCount":"2"},"inputConfig":{"fileName":"files/` + providerOwned + `-2"}` + extra + `}}`
	}
	replies := geminiReplies(request, nth)
	const finalize = "POST /upload/session-" + providerOwned
	replies[finalize] = strings.Replace(replies[finalize], "files/"+providerOwned, fmt.Sprintf("files/%s-%d", providerOwned, nth), 1)
	replies["POST /models/gemini-model:batchGenerateContent"] = batch("BATCH_STATE_PENDING", "")
	replies["GET /"+native] = batch("BATCH_STATE_SUCCEEDED", `,"output":{"responsesFile":"files/output-`+providerOwned+`"}`)
	replies["POST /"+native+":cancel"] = `{}`
	replies["DELETE /"+native] = `{}`
	return replies
}

type geminiBatch struct {
	Name     string
	Metadata struct {
		Model, State string
		InputConfig  struct{ FileName string }
		Output       struct{ ResponsesFile string }
	}
}

func geminiBatchLine(key string) string {
	return `{"key":"` + key + `","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
}

func (g *storedGateway) uploadGeminiFile(t *testing.T, key, contentType, content string) geminiFile {
	t.Helper()
	started, _ := g.send(t, "POST", "/gemini/upload/v1beta/files", geminiKey(key, map[string]string{
		"Content-Type": "application/json", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
		"X-Goog-Upload-Header-Content-Length": strconv.Itoa(len(content)), "X-Goog-Upload-Header-Content-Type": contentType,
	}), `{"file":{"display_name":"notes"}}`, 200)
	session, found := strings.CutPrefix(started.Get("X-Goog-Upload-URL"), g.server.URL)
	if !found {
		t.Fatalf("upload session is %q, want a session on the gateway", started.Get("X-Goog-Upload-URL"))
	}
	_, raw := g.send(t, "POST", session, geminiKey(key, map[string]string{"X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"}), content, 200)
	var uploaded struct{ File geminiFile }
	if err := json.Unmarshal(raw, &uploaded); err != nil {
		t.Fatal(err)
	}
	return uploaded.File
}

func TestGeminiBatchesBindTheirInputAndOutputFilesHideProviderIdentifiersAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug = "gemini-batches"
	const create = "/gemini/v1beta/models/" + slug + ":batchGenerateContent"
	const native = "batches/batch-" + providerOwned
	upstream := &retainedUpstream{replies: geminiBatchReplies, headers: geminiUploadHeaders}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "gemini-model", kind: "gemini", surface: "gemini", version: "/v1beta", secure: true,
		operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	owner := geminiKey("owner", map[string]string{"Content-Type": "application/json"})
	stranger := geminiKey("stranger", map[string]string{"Content-Type": "application/json"})
	fromFile := func(name string) string {
		return `{"batch":{"display_name":"nightly","input_config":{"file_name":"` + name + `"}}}`
	}
	inline := func(first, second string) string {
		item := func(key string) string {
			return `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]},"metadata":{"key":"` + key + `"}}`
		}
		return `{"batch":{"display_name":"nightly","input_config":{"requests":{"requests":[` + item(first) + `,` + item(second) + `]}}}}`
	}

	g.send(t, "POST", create, owner, inline("a", "a"), 400)
	g.send(t, "POST", create, geminiKey("owner", map[string]string{"X-OLP-Route": "another-route"}), inline("a", "b"), 400)
	prose := g.uploadGeminiFile(t, "owner", "text/plain", "hello")
	g.send(t, "POST", create, owner, fromFile(prose.Name), 422)
	input := g.uploadGeminiFile(t, "owner", "application/jsonl", geminiBatchLine("a")+"\n"+geminiBatchLine("b")+"\n")
	g.send(t, "POST", create, stranger, fromFile(input.Name), 404)
	repeated := g.uploadGeminiFile(t, "owner", "application/jsonl", geminiBatchLine("a")+"\n"+geminiBatchLine("a")+"\n")
	g.send(t, "POST", create, owner, fromFile(repeated.Name), 422)
	if sent := upstream.sent("POST /models/gemini-model:batchGenerateContent"); len(sent) != 0 {
		t.Fatalf("refused batches reached the provider: %+v", sent)
	}

	var created geminiBatch
	_, raw := g.send(t, "POST", create, owner, fromFile(input.Name), 200)
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Name, "batches/gemini_batch_") || created.Metadata.Model != "models/"+slug || created.Metadata.InputConfig.FileName != input.Name {
		t.Fatalf("created batch is %s, want a gateway batch on the route naming the caller's input file", raw)
	}
	sent := upstream.sent("POST /models/gemini-model:batchGenerateContent")
	if len(sent) != 1 || sent[0].key != "listed-secret" || !strings.Contains(sent[0].body, `"fileName":"files/`+providerOwned+`-2"`) || strings.Contains(sent[0].body, input.Name) {
		t.Fatalf("provider received %+v, want one batch under the provider credential naming the provider's file", sent)
	}

	batch := "/gemini/v1beta/" + created.Name
	g.send(t, "GET", batch, stranger, "", 404)
	g.send(t, "POST", batch+":cancel", stranger, "", 404)
	g.send(t, "DELETE", batch, stranger, "", 404)
	if _, listed := g.send(t, "GET", "/gemini/v1beta/batches", stranger, "", 200); strings.Contains(string(listed), created.Name) {
		t.Fatalf("another key listed the batch: %s", listed)
	}

	var finished geminiBatch
	_, raw = g.send(t, "GET", batch, owner, "", 200)
	if err := json.Unmarshal(raw, &finished); err != nil {
		t.Fatal(err)
	}
	output := finished.Metadata.Output.ResponsesFile
	if finished.Name != created.Name || finished.Metadata.State != "BATCH_STATE_SUCCEEDED" || !strings.HasPrefix(output, "files/gemini_file_") || output == input.Name {
		t.Fatalf("finished batch is %s, want the same batch with a gateway output file of its own", raw)
	}
	var again geminiBatch
	_, raw = g.send(t, "GET", batch, owner, "", 200)
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if again.Metadata.Output.ResponsesFile != output {
		t.Fatalf("output file changed from %q to %q between reads", output, again.Metadata.Output.ResponsesFile)
	}
	g.send(t, "GET", "/gemini/v1beta/"+output, stranger, "", 404)
	if _, listed := g.send(t, "GET", "/gemini/v1beta/batches", owner, "", 200); !strings.Contains(string(listed), created.Name) || !strings.Contains(string(listed), output) {
		t.Fatalf("owner listing is %s, want the batch and its output file", listed)
	}

	g.send(t, "POST", batch+":cancel", owner, "", 200)
	g.send(t, "DELETE", batch, owner, "", 409)
	if cancelled, deleted := upstream.sent("POST /"+native+":cancel"), upstream.sent("DELETE /"+native); len(cancelled) != 1 || len(deleted) != 0 {
		t.Fatalf("provider saw %d cancels and %d deletes, want one cancel and no delete while the batch is unsettled", len(cancelled), len(deleted))
	}
}
