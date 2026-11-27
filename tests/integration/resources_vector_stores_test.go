//go:build integration

package integration_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/tests/harness"
)

func vectorStoreReplies(string, int) map[string]string {
	store, path := `{"id":"vs_`+providerOwned+`","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`, "/vector_stores/vs_"+providerOwned
	file := `{"id":"file-` + providerOwned + `","object":"vector_store.file","vector_store_id":"vs_` + providerOwned + `","status":"completed","created_at":1}`
	return map[string]string{
		"POST /files":             `{"id":"file-` + providerOwned + `","object":"file","bytes":5,"created_at":1,"filename":"a.txt","purpose":"assistants","status":"processed"}`,
		"POST /vector_stores":     store,
		"GET " + path:             store,
		"POST " + path:            store,
		"DELETE " + path:          `{"id":"vs_` + providerOwned + `","object":"vector_store.deleted","deleted":true}`,
		"POST " + path + "/files": file,
		"GET " + path + "/files":  `{"object":"list","data":[` + file + `],"has_more":false}`,
		"GET " + path + "/files/file-" + providerOwned: file,
	}
}

func TestVectorStoresHideProviderIdentifiersBindFilesReplayCreationAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	upstream := &retainedUpstream{replies: vectorStoreReplies}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const stores = "/v1/vector_stores"
	submit := func(key, path, contentType, body, submission string, want int) retainedObject {
		t.Helper()
		_, raw := g.send(t, "POST", path, map[string]string{"Authorization": "Bearer " + key, "Content-Type": contentType, "Idempotency-Key": submission}, body, want)
		var decoded retainedObject
		_ = json.Unmarshal(raw, &decoded)
		return decoded
	}

	upload := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	file := submit("owner", "/v1/files", "multipart/form-data; boundary=b", upload, uuid.NewString(), 200)
	creation := uuid.NewString()
	store := submit("owner", stores, "application/json", `{"name":"notes"}`, creation, 200)
	if file.ID == "" || store.ID == "" {
		t.Fatalf("creation returned file %q and store %q, want gateway identifiers", file.ID, store.ID)
	}
	replayed, _ := g.send(t, "POST", stores, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": creation}, `{"name":"notes"}`, 409)
	if got := replayed.Get("X-OLP-Resource-ID"); got != store.ID {
		t.Fatalf("a replayed creation points at %q, want %q", got, store.ID)
	}
	if created := upstream.sent("POST /vector_stores"); len(created) != 1 || created[0].key != "listed-secret" {
		t.Fatalf("the provider saw creations %+v, want one under the provider credential", created)
	}
	own := stores + "/" + store.ID
	if got := g.call(t, "owner", "GET", own, "", "", 200); got.ID != store.ID {
		t.Fatalf("store read returned %q, want %q", got.ID, store.ID)
	}
	if got := g.call(t, "owner", "GET", stores, "", "", 200).identifiers(); !slices.Equal(got, []string{store.ID}) {
		t.Fatalf("owner lists stores %v, want %v", got, []string{store.ID})
	}
	if got := submit("owner", own, "application/json", `{"name":"renamed"}`, "", 200); got.ID != store.ID {
		t.Fatalf("store update returned %q, want %q", got.ID, store.ID)
	}

	attached := submit("owner", own+"/files", "application/json", `{"file_id":"`+file.ID+`"}`, uuid.NewString(), 200)
	if attached.ID == "" || attached.VectorStoreID != store.ID {
		t.Fatalf("attachment = %+v, want a gateway identifier inside store %q", attached, store.ID)
	}
	bound := upstream.sent("POST /vector_stores/vs_" + providerOwned + "/files")
	if len(bound) != 1 || !strings.Contains(bound[0].body, `"file_id":"file-`+providerOwned+`"`) || strings.Contains(bound[0].body, file.ID) {
		t.Fatalf("attachment reached the provider as %+v, want the provider's file identifier", bound)
	}
	if got := g.call(t, "owner", "GET", own+"/files/"+attached.ID, "", "", 200); got.ID != attached.ID || got.VectorStoreID != store.ID {
		t.Fatalf("attachment read = %+v, want %q inside %q", got, attached.ID, store.ID)
	}
	if got := g.call(t, "owner", "GET", own+"/files", "", "", 200).identifiers(); !slices.Equal(got, []string{attached.ID}) {
		t.Fatalf("owner lists attachments %v, want %v", got, []string{attached.ID})
	}

	seen := len(upstream.sent("GET /vector_stores/vs_"+providerOwned)) + len(upstream.sent("POST /vector_stores/vs_"+providerOwned+"/files"))
	if got := g.call(t, "stranger", "GET", stores, "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists stores %v", got)
	}
	g.call(t, "stranger", "GET", own, "", "", 404)
	g.call(t, "stranger", "GET", own+"/files", "", "", 404)
	g.call(t, "stranger", "GET", own+"/files/"+attached.ID, "", "", 404)
	g.call(t, "stranger", "DELETE", own, "", "", 404)
	submit("stranger", own, "application/json", `{"name":"taken"}`, "", 404)
	submit("stranger", own+"/files", "application/json", `{"file_id":"`+file.ID+`"}`, uuid.NewString(), 404)
	if after := len(upstream.sent("GET /vector_stores/vs_"+providerOwned)) + len(upstream.sent("POST /vector_stores/vs_"+providerOwned+"/files")); after != seen {
		t.Fatalf("another key's requests reached the provider %d times", after-seen)
	}

	if got := g.call(t, "owner", "DELETE", own, "", "", 200); got.ID != store.ID || !got.Deleted {
		t.Fatalf("store delete = %+v", got)
	}
	if sent := upstream.sent("DELETE /vector_stores/vs_" + providerOwned); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want 1", len(sent))
	}
	replacement := submit("owner", stores, "application/json", `{"name":"notes"}`, uuid.NewString(), 200)
	if replacement.ID == "" || replacement.ID == store.ID {
		t.Fatalf("creation with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
	}
	g.call(t, "owner", "GET", own, "", "", 404)
	g.call(t, "owner", "GET", own+"/files/"+attached.ID, "", "", 404)
	if got := g.call(t, "owner", "GET", stores, "", "", 200).identifiers(); !slices.Equal(got, []string{replacement.ID}) {
		t.Fatalf("owner lists stores %v, want only %s", got, replacement.ID)
	}
	replayedAfterDelete, _ := g.send(t, "POST", stores, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": creation}, `{"name":"notes"}`, 409)
	if got := replayedAfterDelete.Get("X-OLP-Resource-ID"); got != store.ID {
		t.Fatalf("replaying the first creation returned %q, want the original %q", got, store.ID)
	}
}

func twoStoreReplies(request string, nth int) map[string]string {
	replies := map[string]string{
		"POST /files":         `{"id":"file-` + providerOwned + `","object":"file","bytes":5,"created_at":1,"filename":"a.txt","purpose":"assistants","status":"processed"}`,
		"POST /vector_stores": `{"id":"vs_` + providerOwned + strconv.Itoa(nth) + `","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`,
	}
	for _, n := range []string{"1", "2"} {
		store, path := `{"id":"vs_`+providerOwned+n+`","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`, "/vector_stores/vs_"+providerOwned+n
		file := `{"id":"file-` + providerOwned + `","object":"vector_store.file","vector_store_id":"vs_` + providerOwned + n + `","status":"completed","created_at":1}`
		replies["GET "+path], replies["POST "+path] = store, store
		replies["DELETE "+path] = `{"id":"vs_` + providerOwned + n + `","object":"vector_store.deleted","deleted":true}`
		replies["POST "+path+"/files"], replies["GET "+path+"/files/file-"+providerOwned] = file, file
	}
	return replies
}

func TestVectorStoreRequestsNeedASubmissionKeyStayInsideTheirStoreAndLoseOnlyNewWorkWhenTheRouteDropsTheOperation(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	upstream := &retainedUpstream{replies: twoStoreReplies}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stateless")
	g.key(t, "elsewhere")
	stateless, elsewhere := g.runtime.keys["stateless"], g.runtime.keys["elsewhere"]
	stateless.Policy.AllowProviderState, elsewhere.Policy.AllowedRoutes = false, []string{"another-route"}
	g.runtime.keys["stateless"], g.runtime.keys["elsewhere"] = stateless, elsewhere

	const stores, named, paid = "/v1/vector_stores", `{"name":"notes"}`, `{"expires_after":{"anchor":"last_active_at","days":1}}`
	ask := func(key, method, path, body string, headers map[string]string, want int) (http.Header, retainedObject, string) {
		t.Helper()
		sent := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
		maps.Copy(sent, headers)
		header, raw := g.send(t, method, path, sent, body, want)
		var decoded retainedObject
		var refusal struct{ Error struct{ Code string } }
		_ = json.Unmarshal(raw, &decoded)
		_ = json.Unmarshal(raw, &refusal)
		return header, decoded, refusal.Error.Code
	}
	submitted := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }
	refused := func(situation, key, method, path, body string, headers map[string]string, status int, code string) http.Header {
		t.Helper()
		header, _, got := ask(key, method, path, body, headers, status)
		if got != code {
			t.Fatalf("%s is refused with %q, want %q", situation, got, code)
		}
		return header
	}

	for situation, attempt := range map[string]struct {
		key     string
		headers map[string]string
		status  int
		code    string
	}{
		"a creation without a submission key":    {"owner", nil, 400, "missing_required_parameter"},
		"a key that may not keep provider state": {"stateless", submitted(), 403, "provider_state_forbidden"},
		"a key that may not use the route":       {"elsewhere", submitted(), 403, "route_forbidden"},
	} {
		if header := refused(situation, attempt.key, "POST", stores, named, attempt.headers, attempt.status, attempt.code); header.Get("X-Should-Retry") != "false" {
			t.Fatalf("%s carries retry hint %q, want a creation refusal that says not to retry", situation, header.Get("X-Should-Retry"))
		}
	}
	if created := upstream.sent("POST /vector_stores"); len(created) != 0 {
		t.Fatalf("refused creations reached the provider %d times", len(created))
	}

	upload := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	_, file, _ := ask("owner", "POST", "/v1/files", upload, map[string]string{"Content-Type": "multipart/form-data; boundary=b", "Idempotency-Key": uuid.NewString()}, 200)
	_, first, _ := ask("owner", "POST", stores, named, submitted(), 200)
	_, second, _ := ask("owner", "POST", stores, named, submitted(), 200)
	if created := upstream.sent("POST /vector_stores"); len(created) != 2 || !strings.Contains(created[0].body, `"olp_submission_id":"`) {
		t.Fatalf("the provider saw creations %+v, want two that carry the gateway's submission identifier", created)
	}

	inFirst, inSecond := stores+"/"+first.ID, stores+"/"+second.ID
	attachment := submitted()
	_, attached, _ := ask("owner", "POST", inFirst+"/files", `{"file_id":"`+file.ID+`"}`, attachment, 200)
	refused("the same submission key in another store", "owner", "POST", inSecond+"/files", `{"file_id":"`+file.ID+`"}`, attachment, 409, "idempotency_conflict")
	refused("an attachment read through another store", "owner", "GET", inSecond+"/files/"+attached.ID, "", nil, 404, "not_found")
	if _, other, _ := ask("owner", "POST", inSecond+"/files", `{"file_id":"`+file.ID+`"}`,
		map[string]string{"X-OLP-Route": "another-route", "Idempotency-Key": uuid.NewString()}, 200); other.VectorStoreID != second.ID {
		t.Fatalf("an attachment under a foreign route header landed in %q, want its store %q whatever the header says", other.VectorStoreID, second.ID)
	}

	g.republish(t, func(route *routing.Route) { route.Operations = []string{"generation", "batch"} })
	if header := refused("a creation on a route without the operation", "owner", "POST", stores, named, submitted(), 422, "operation_unavailable"); header.Get("X-Should-Retry") != "false" {
		t.Fatalf("a refused creation carries retry hint %q", header.Get("X-Should-Retry"))
	}
	refused("paid work on a store whose route dropped the operation", "owner", "POST", inFirst, paid, submitted(), 409, "provider_resource_credential_unavailable")
	if _, read, _ := ask("owner", "GET", inFirst, "", nil, 200); read.ID != first.ID {
		t.Fatalf("a store whose route dropped the operation reads as %q, want %q", read.ID, first.ID)
	}
	if _, renamed, _ := ask("owner", "POST", inFirst, `{"name":"renamed"}`, nil, 200); renamed.ID != first.ID {
		t.Fatalf("renaming a store whose route dropped the operation returned %q, want %q", renamed.ID, first.ID)
	}
	g.republish(t, func(route *routing.Route) { route.Operations = []string{"generation", "batch", "vector_store"} })
	refused("paid work without a submission key", "owner", "POST", inFirst, paid, nil, 400, "invalid_idempotency_key")
	if _, worked, _ := ask("owner", "POST", inFirst, paid, submitted(), 200); worked.ID != first.ID {
		t.Fatalf("paid work on a store returned %q, want %q", worked.ID, first.ID)
	}

	before := len(upstream.sent("GET /vector_stores/vs_" + providerOwned + "2"))
	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_resources SET state='submission_unknown' WHERE replace(id::text,'-','')=$1`,
		strings.TrimPrefix(second.ID, "vector_store_")); err != nil {
		t.Fatal(err)
	}
	refused("a store whose creation was never reconciled", "owner", "GET", inSecond, "", nil, 409, "submission_unknown")
	if after := len(upstream.sent("GET /vector_stores/vs_" + providerOwned + "2")); after != before {
		t.Fatalf("a refused read reached the provider %d times", after-before)
	}
}

func providerStore(n string) string {
	return `{"id":"vs_` + providerOwned + n + `","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`
}

func providerStorePath(n string) string { return "/vector_stores/vs_" + providerOwned + n }

func storeObjects(issues string) map[string]string {
	replies := map[string]string{"POST /vector_stores": providerStore(issues)}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		replies["GET "+providerStorePath(n)], replies["POST "+providerStorePath(n)] = providerStore(n), providerStore(n)
		replies["DELETE "+providerStorePath(n)] = `{"id":"vs_` + providerOwned + n + `","object":"vector_store.deleted","deleted":true}`
	}
	return replies
}

func TestAVectorStoreProviderAnswerDecidesWhatIsKeptWhatIsForgottenAndWhatTheCallerMayRepeat(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	ask := func(method, path, body, submission string, status int, code, class string) (http.Header, retainedObject) {
		t.Helper()
		before := len(sink.all())
		header, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": submission}, body, status)
		var reply struct {
			retainedObject
			Error struct{ Code string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil || reply.Error.Code != code {
			t.Fatalf("%s %s answered %s (%v), want code %q", method, path, raw, err, code)
		}
		seen := sink.all()
		if len(seen) != before+1 {
			t.Fatalf("%s %s recorded %d requests, want one", method, path, len(seen)-before)
		}
		attempts := seen[before].Attempts
		if class == "" && len(attempts) != 0 || class != "" && (len(attempts) != 1 || attempts[0].Class != class) {
			t.Fatalf("%s %s recorded attempts %+v, want class %q", method, path, attempts, class)
		}
		return header, reply.retainedObject
	}
	kept := func(id string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state FROM olp.provider_resources WHERE 'vector_store_'||replace(id::text,'-','')=$1`, id).Scan(&state); err != nil {
			t.Fatalf("read kept store %s: %v", id, err)
		}
		return state
	}
	change := func(id, submission string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT m.state FROM olp.retained_mutations m WHERE 'vector_store_'||replace(m.resource_id::text,'-','')=$1 AND m.submission_key=$2`, id, submission).Scan(&state); err != nil {
			t.Fatalf("read change %s of %s: %v", submission, id, err)
		}
		return state
	}
	const stores, named, paid = "/v1/vector_stores", `{"name":"notes"}`, `{"expires_after":{"anchor":"last_active_at","days":1}}`
	rejection, outage := `{"error":{"message":"The provider refused.","type":"invalid_request_error"}}`, `{"error":{"message":"The provider broke.","type":"server_error"}}`

	provider.answers("POST /vector_stores", http.StatusBadRequest, rejection)
	header, _ := ask("POST", stores, named, "refused", http.StatusBadRequest, "provider_resource_error", "upstream_client")
	refused := header.Get("X-OLP-Resource-ID")
	if header.Get("X-Should-Retry") != "false" || kept(refused) != "not_accepted" {
		t.Fatalf("a refused creation answered retry %q and is kept as %q", header.Get("X-Should-Retry"), kept(refused))
	}
	provider.recovers("POST /vector_stores")
	header, _ = ask("POST", stores, named, "refused", http.StatusConflict, "submission_exists", "")
	if header.Get("X-OLP-Resource-ID") != refused || len(upstream.sent("POST /vector_stores")) != 1 {
		t.Fatalf("repeating a refused creation named %q and reached the provider %d times", header.Get("X-OLP-Resource-ID"), len(upstream.sent("POST /vector_stores")))
	}

	provider.answers("POST /vector_stores", http.StatusInternalServerError, outage)
	header, _ = ask("POST", stores, named, "unknown", http.StatusInternalServerError, "provider_resource_error", "ambiguous")
	unknown := header.Get("X-OLP-Resource-ID")
	if kept(unknown) != "submission_unknown" {
		t.Fatalf("a creation the provider may have run is kept as %q", kept(unknown))
	}
	provider.recovers("POST /vector_stores")
	ask("GET", stores+"/"+unknown, "", "", http.StatusConflict, "submission_unknown", "")

	_, store := ask("POST", stores, named, "first", http.StatusOK, "", "success")
	for _, broken := range []struct {
		name, body string
		status     int
		code       string
		class      string
		want       int
	}{
		{"another store", providerStore("2"), 0, "provider_protocol_error", "protocol", http.StatusBadGateway},
		{"no object", `[]`, 0, "provider_protocol_error", "protocol", http.StatusBadGateway},
		{"an outage", outage, http.StatusInternalServerError, "provider_resource_error", "upstream_server", http.StatusInternalServerError},
	} {
		provider.answers("GET "+providerStorePath("1"), broken.status, broken.body)
		ask("GET", stores+"/"+store.ID, "", "", broken.want, broken.code, broken.class)
		if kept(store.ID) != "completed" {
			t.Fatalf("a read answered with %s left the store %q", broken.name, kept(store.ID))
		}
	}
	provider.recovers("GET " + providerStorePath("1"))

	ask("POST", stores+"/"+store.ID, paid, "accepted", http.StatusOK, "", "success")
	ask("POST", stores+"/"+store.ID, paid, "accepted", http.StatusConflict, "idempotency_conflict", "")
	provider.answers("POST "+providerStorePath("1"), http.StatusBadRequest, rejection)
	ask("POST", stores+"/"+store.ID, paid, "rejected", http.StatusBadRequest, "provider_resource_error", "upstream_client")
	provider.answers("POST "+providerStorePath("1"), http.StatusInternalServerError, outage)
	ask("POST", stores+"/"+store.ID, paid, "uncertain", http.StatusInternalServerError, "provider_resource_error", "ambiguous")
	provider.recovers("POST " + providerStorePath("1"))
	ask("POST", stores+"/"+store.ID, paid, "uncertain", http.StatusConflict, "idempotency_conflict", "")
	for submission, want := range map[string]string{"accepted": "accepted", "rejected": "not_accepted", "uncertain": "submission_unknown"} {
		if got := change(store.ID, submission); got != want {
			t.Fatalf("paid change %q is kept as %q, want %q", submission, got, want)
		}
	}
	if kept(store.ID) != "completed" {
		t.Fatalf("failed paid changes left the store %q", kept(store.ID))
	}

	provider.answers("DELETE "+providerStorePath("1"), 0, `{"id":"vs_`+providerOwned+`2","object":"vector_store.deleted","deleted":true}`)
	ask("DELETE", stores+"/"+store.ID, "", "", http.StatusBadGateway, "provider_protocol_error", "protocol")
	if kept(store.ID) != "deleted" {
		t.Fatalf("a deletion the provider accepted under another name left the store %q", kept(store.ID))
	}

	provider.creates("2")
	_, second := ask("POST", stores, named, "second", http.StatusOK, "", "success")
	ask("DELETE", stores+"/"+second.ID, "", "", http.StatusOK, "", "success")
	if kept(second.ID) != "deleted" {
		t.Fatalf("a deleted store is kept as %q", kept(second.ID))
	}

	provider.creates("3")
	_, gone := ask("POST", stores, named, "third", http.StatusOK, "", "success")
	provider.answers("GET "+providerStorePath("3"), http.StatusNotFound, rejection)
	ask("GET", stores+"/"+gone.ID, "", "", http.StatusNotFound, "not_found", "upstream_client")
	provider.recovers("GET " + providerStorePath("3"))
	ask("GET", stores+"/"+gone.ID, "", "", http.StatusNotFound, "not_found", "")
	if kept(gone.ID) != "deleted" || len(upstream.sent("GET "+providerStorePath("3"))) != 1 {
		t.Fatalf("a store the provider no longer has is kept as %q after %d provider reads", kept(gone.ID), len(upstream.sent("GET "+providerStorePath("3"))))
	}
}

type storePage struct {
	status      int
	stores      string
	more        bool
	first, last string
	code        string
	reads       int
	attempts    string
}

func TestAVectorStoreListPagesNewestFirstRefillsPastLostStoresAndRefusesWhatItCannotPage(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	numbered, issued := map[string]string{}, map[string]string{}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		provider.creates(n)
		_, raw := g.send(t, "POST", "/v1/vector_stores", map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": "store-" + n}, `{"name":"notes"}`, http.StatusOK)
		var store retainedObject
		if err := json.Unmarshal(raw, &store); err != nil || store.ID == "" {
			t.Fatalf("create store %s answered %s: %v", n, raw, err)
		}
		numbered[store.ID], issued[n] = n, store.ID
	}
	reads := func() int {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(upstream.calls), func(call upstreamCall) bool { return !strings.HasPrefix(call.request, "GET ") }))
	}
	list := func(query, route string, want int) storePage {
		t.Helper()
		recorded, read := len(sink.all()), reads()
		_, raw := g.send(t, "GET", "/v1/vector_stores"+query, map[string]string{"Authorization": "Bearer owner", "X-OLP-Route": route}, "", want)
		var reply struct {
			Data    []struct{ ID string }
			HasMore bool   `json:"has_more"`
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
			Error   struct{ Code string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("list %s answered %s: %v", query, raw, err)
		}
		page := storePage{status: want, more: reply.HasMore, first: numbered[reply.FirstID], last: numbered[reply.LastID], code: reply.Error.Code, reads: reads() - read}
		for _, store := range reply.Data {
			page.stores += numbered[store.ID]
		}
		seen := sink.all()
		if len(seen) != recorded+1 {
			t.Fatalf("list %s recorded %d requests, want one", query, len(seen)-recorded)
		}
		for _, attempt := range seen[recorded].Attempts {
			page.attempts += fmt.Sprintf("%s:%v ", attempt.Class, attempt.Committed)
		}
		return page
	}
	delivered := func(n int) string { return strings.Repeat("success:true ", n) }
	refused := storePage{status: http.StatusBadRequest, code: "invalid_parameter"}

	for _, page := range []struct {
		name, query string
		want        storePage
	}{
		{"the whole list, newest first", "", storePage{status: 200, stores: "54321", first: "5", last: "1", reads: 5, attempts: delivered(5)}},
		{"a first page", "?limit=2", storePage{status: 200, stores: "54", more: true, first: "5", last: "4", reads: 2, attempts: delivered(2)}},
		{"the page after a store", "?limit=2&after=" + issued["4"], storePage{status: 200, stores: "32", more: true, first: "3", last: "2", reads: 2, attempts: delivered(2)}},
		{"the last page", "?limit=2&after=" + issued["2"], storePage{status: 200, stores: "1", first: "1", last: "1", reads: 1, attempts: delivered(1)}},
		{"a page that ends with the list", "?limit=5", storePage{status: 200, stores: "54321", first: "5", last: "1", reads: 5, attempts: delivered(5)}},
		{"oldest first", "?order=asc&limit=3", storePage{status: 200, stores: "123", more: true, first: "1", last: "3", reads: 3, attempts: delivered(3)}},
		{"an unknown parameter", "?filter=notes", refused},
		{"a repeated parameter", "?limit=2&limit=3", refused},
		{"an unknown order", "?order=sideways", refused},
		{"an empty page", "?limit=0", refused},
		{"a page over the largest size", "?limit=101", refused},
		{"a cursor that is no store of this key", "?after=vector_store_00000000000000000000000000000000", refused},
	} {
		if got := list(page.query, slug, page.want.status); got != page.want {
			t.Fatalf("%s is %+v, want %+v", page.name, got, page.want)
		}
	}
	if got, want := list("", "another-route", 200), (storePage{status: 200}); got != want {
		t.Fatalf("the stores of another route are %+v, want none and no provider read", got)
	}

	for _, n := range []string{"3", "4"} {
		provider.answers("GET "+providerStorePath(n), http.StatusNotFound, `{"error":{"message":"No such store.","type":"invalid_request_error"}}`)
	}
	lost := storePage{status: 200, stores: "52", more: true, first: "5", last: "2", reads: 4, attempts: "success:true upstream_client:false upstream_client:false success:true "}
	if got := list("?limit=2", slug, 200); got != lost {
		t.Fatalf("a page over two stores the provider lost is %+v, want %+v", got, lost)
	}
	forgotten := storePage{status: 200, stores: "52", more: true, first: "5", last: "2", reads: 2, attempts: delivered(2)}
	if got := list("?limit=2", slug, 200); got != forgotten {
		t.Fatalf("the page after the lost stores were forgotten is %+v, want %+v", got, forgotten)
	}

	for _, broken := range []struct {
		name, body string
		status     int
		want       storePage
	}{
		{"another store", providerStore("1"), 0, storePage{status: http.StatusBadGateway, code: "provider_protocol_error", reads: 2, attempts: "success:false protocol:false "}},
		{"an outage", `{"error":{"message":"The provider broke.","type":"server_error"}}`, http.StatusInternalServerError, storePage{status: http.StatusInternalServerError, code: "provider_resource_error", reads: 2, attempts: "success:false upstream_server:false "}},
	} {
		provider.answers("GET "+providerStorePath("2"), broken.status, broken.body)
		if got := list("", slug, broken.want.status); got != broken.want {
			t.Fatalf("a list whose second store is answered with %s is %+v, want %+v", broken.name, got, broken.want)
		}
	}
	provider.recovers("GET " + providerStorePath("2"))

	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_resources SET state='submission_unknown' WHERE 'vector_store_'||replace(id::text,'-','')=$1`, issued["2"]); err != nil {
		t.Fatal(err)
	}
	unsettled := storePage{status: 200, stores: "51", first: "5", last: "1", reads: 2, attempts: delivered(2)}
	if got := list("", slug, 200); got != unsettled {
		t.Fatalf("a list over a store whose creation is unknown is %+v, want %+v", got, unsettled)
	}
}

func TestStoreSearchAndCompactionTranslateFileIdentifiersBothWaysAccountTheirWorkAndRefuseWhatTheyCannotBind(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store", "retrieval"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	g.key(t, "stateless")
	g.key(t, "elsewhere")
	stateless, elsewhere := g.runtime.keys["stateless"], g.runtime.keys["elsewhere"]
	stateless.Policy.AllowProviderState, elsewhere.Policy.AllowedRoutes = false, []string{"another-route"}
	g.runtime.keys["stateless"], g.runtime.keys["elsewhere"] = stateless, elsewhere
	const providerFile = "file-" + providerOwned
	provider.answers("POST /files", 200, `{"id":"`+providerFile+`","object":"file","bytes":5,"created_at":1,"filename":"a.txt","purpose":"assistants","status":"processed"}`)
	provider.answers("POST "+providerStorePath("1")+"/files", 200, `{"id":"`+providerFile+`","object":"vector_store.file","vector_store_id":"vs_`+providerOwned+`1","status":"completed","created_at":1}`)
	created := func(path, contentType, body, submission string) string {
		t.Helper()
		_, raw := g.send(t, "POST", path, map[string]string{"Authorization": "Bearer owner", "Content-Type": contentType, "Idempotency-Key": submission}, body, http.StatusOK)
		var object retainedObject
		if err := json.Unmarshal(raw, &object); err != nil || object.ID == "" {
			t.Fatalf("POST %s answered %s: %v", path, raw, err)
		}
		return object.ID
	}
	const form = "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	file := created("/v1/files", "multipart/form-data; boundary=b", form, "file")
	store := created("/v1/vector_stores", "application/json", `{"name":"notes"}`, "store")
	created("/v1/vector_stores/"+store+"/files", "application/json", `{"file_id":"`+file+`"}`, "attachment")

	const search, compact = "/search", "/v1/responses/compact"
	searched, compacted := "POST "+providerStorePath("1")+search, "POST /responses/compact"
	found := func(fileID string) string {
		return `{"object":"vector_store.search_results.page","search_query":["q"],"data":[{"file_id":"` + fileID + `","score":0.5,"content":[{"type":"text","text":"hello"}]}],"has_more":false}`
	}
	const usage = `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}`
	summary := func(item, usage string) string {
		return `{"object":"response.compaction","id":"cmp_1","output":[` + item + `]` + usage + `}`
	}
	const kept = `{"type":"compaction","id":"item_1","encrypted_content":"abc"}`
	attached := func(fileID string) string { return `{"type":"input_file","file_id":"` + fileID + `"}` }
	about := func(fileID string) string {
		return `{"model":"` + slug + `","input":[{"role":"user","content":[` + attached(fileID) + `]}]}`
	}
	const question, text = `{"query":"q"}`, `{"model":"` + slug + `","input":"shorten this"}`
	type work struct {
		name, key, path, query, route, body string
		answer                              string
		answers                             int
		status                              int
		code                                string
		reply, sent                         string
		attempt                             string
		tokens                              int64
	}
	for _, tc := range []work{
		{name: "a search names the caller's file", path: search, body: question, answer: found(providerFile), status: 200, reply: `"file_id":"` + file + `"`, sent: question, attempt: "success committed tokenless"},
		{name: "a search naming a file the caller never bound", path: search, body: question, answer: found("file-elsewhere"), status: 502, code: "provider_protocol_error", attempt: "protocol"},
		{name: "a search the provider cannot answer", path: search, body: question, answer: `{"error":{"message":"down"}}`, answers: 500, status: 500, code: "provider_resource_error", attempt: "ambiguous"},
		{name: "a search the provider throttles", path: search, body: question, answer: `{"error":{"message":"slow"}}`, answers: 429, status: 429, code: "provider_resource_error", attempt: "rate_limit"},
		{name: "a search answered with a list", path: search, body: question, answer: `[]`, status: 502, code: "provider_protocol_error", attempt: "protocol"},
		{name: "a search with a query parameter", path: search, query: "?limit=1", body: question, status: 400, code: "invalid_parameter"},
		{name: "a search that is not an object", path: search, body: `[1]`, status: 400, code: "invalid_request"},
		{name: "another key's search", key: "stranger", path: search, body: question, status: 404, code: "not_found"},
		{name: "a compaction is accounted by its tokens", path: compact, route: slug, body: text, answer: summary(kept, usage), status: 200, reply: `"encrypted_content":"abc"`, sent: `{"model":"` + model + `","input":"shorten this"}`, attempt: "success committed", tokens: 15},
		{name: "a compaction over the caller's file", path: compact, route: slug, body: about(file), answer: summary(kept, usage), status: 200, sent: attached(providerFile), attempt: "success committed", tokens: 15},
		{name: "a compaction returning the caller's file", path: compact, route: slug, body: text, answer: summary(attached(providerFile), usage), status: 200, reply: attached(file), attempt: "success committed", tokens: 15},
		{name: "a compaction returning a file the caller never bound keeps its usage", path: compact, route: slug, body: text, answer: summary(attached("file-elsewhere"), usage), status: 502, code: "provider_protocol_error", attempt: "protocol", tokens: 15},
		{name: "a compaction with usage that cannot be read", path: compact, route: slug, body: text, answer: summary(kept, `,"usage":{"input_tokens":-1}`), status: 502, code: "provider_protocol_error", attempt: "protocol"},
		{name: "a compaction the provider cannot answer", path: compact, route: slug, body: text, answer: `{"error":{"message":"down"}}`, answers: 500, status: 500, code: "provider_resource_error", attempt: "ambiguous"},
		{name: "a compaction over a file the caller does not own", path: compact, route: slug, body: about("file_00000000000040008000000000000000"), status: 400, code: "invalid_resource_reference"},
		{name: "a compaction after a response the caller does not own", path: compact, route: slug, body: `{"input":"x","previous_response_id":"response_00000000000040008000000000000000"}`, status: 400, code: "invalid_resource_reference"},
		{name: "a compaction by a key that may not keep provider state", key: "stateless", path: compact, route: slug, body: text, status: 403, code: "provider_state_forbidden"},
		{name: "a compaction by a key that may not use the route", key: "elsewhere", path: compact, route: slug, body: text, status: 403, code: "route_forbidden"},
		{name: "a compaction on an unknown route", path: compact, route: "elsewhere", body: text, status: 404, code: "route_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, request := tc.path, compacted
			if tc.path == search {
				path, request = "/v1/vector_stores/"+store+search, searched
			}
			if tc.answer != "" {
				provider.answers(request, cmp.Or(tc.answers, 200), tc.answer)
			}
			recorded, asked := len(sink.all()), len(upstream.sent(request))
			_, raw := g.send(t, "POST", path+tc.query, map[string]string{"Authorization": "Bearer " + cmp.Or(tc.key, "owner"), "Content-Type": "application/json", "X-OLP-Route": tc.route}, tc.body, tc.status)
			var reply struct{ Error struct{ Code string } }
			if err := json.Unmarshal(raw, &reply); err != nil || reply.Error.Code != tc.code || !strings.Contains(string(raw), tc.reply) {
				t.Fatalf("reply = %s (%v), want code %q containing %q", raw, err, tc.code, tc.reply)
			}
			calls := upstream.sent(request)[asked:]
			if (tc.answer != "") != (len(calls) == 1) || (tc.sent != "" && !strings.Contains(calls[0].body, tc.sent)) {
				t.Fatalf("the provider received %+v, want a request only when it answers, containing %q", calls, tc.sent)
			}
			for _, call := range calls {
				if call.key != "listed-secret" || strings.Contains(call.body, file) || strings.Contains(call.body, store) {
					t.Fatalf("the provider received %+v, want the provider credential and no gateway identifier", call)
				}
			}
			seen := sink.all()
			if len(seen) != recorded+1 {
				t.Fatalf("recorded %d requests, want one", len(seen)-recorded)
			}
			got, tokens := "", int64(0)
			for _, attempt := range seen[recorded].Attempts {
				got += attempt.Class
				if attempt.Committed {
					got += " committed"
				}
				if attempt.TokenlessUsage {
					got += " tokenless"
				}
				if attempt.Usage != nil {
					tokens += attempt.Usage.TotalTokens
				}
			}
			if got != tc.attempt || tokens != tc.tokens {
				t.Fatalf("attempts = %q with %d tokens, want %q with %d", got, tokens, tc.attempt, tc.tokens)
			}
		})
	}

	t.Run("a fresh search uses the live mapping after provider ID reuse", func(t *testing.T) {
		provider.answers("DELETE /files/"+providerFile, 200, `{"id":"`+providerFile+`","deleted":true}`)
		g.call(t, "owner", "DELETE", "/v1/files/"+file, "", "", 200)
		replacement := created("/v1/files", "multipart/form-data; boundary=b", form, "replacement-file")
		if replacement == file {
			t.Fatalf("replacement reused deleted gateway file ID %q", file)
		}
		provider.answers(searched, 200, found(providerFile))
		_, raw := g.send(t, "POST", "/v1/vector_stores/"+store+search, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}, question, 200)
		if !strings.Contains(string(raw), `"file_id":"`+replacement+`"`) {
			t.Fatalf("search returned %s, want replacement file %q", raw, replacement)
		}
	})

	unasked := func(path, route, want string) {
		t.Helper()
		asked := len(upstream.sent(searched)) + len(upstream.sent(compacted))
		request, err := http.NewRequest("POST", g.server.URL+path, strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		if route != "" {
			request.Header.Set("X-OLP-Route", route)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if !strings.Contains(string(raw), `"code":"`+want+`"`) || len(upstream.sent(searched))+len(upstream.sent(compacted)) != asked {
			t.Fatalf("%s on route %q answered %d %s, want %s and no provider request", path, route, response.StatusCode, raw, want)
		}
	}
	unasked(compact, "", "missing_required_parameter")
	g.republish(t, func(route *routing.Route) { route.Operations = []string{"vector_store", "batch"} })
	unasked(compact, slug, "operation_unavailable")
	unasked("/v1/vector_stores/"+store+search, slug, "provider_resource_credential_unavailable")
}
