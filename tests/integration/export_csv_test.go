//go:build integration

package integration_test

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (h *accessHarness) download(b *browser, method, path string, headers map[string]string) (int, http.Header, string) {
	h.t.Helper()
	response, raw := h.do(b, method, path, nil, headers)
	response.Body.Close()
	return response.StatusCode, response.Header, string(raw)
}

func ptr[T any](v T) *T { return &v }

func TestExportRequestCSVDownload(t *testing.T) {
	h := repHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "CSV"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	key := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "csv key", "scopes": []string{"inference"}, "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyID := key["id"].(string)
	started := time.Now().Add(-time.Hour)
	requestID := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,total_latency_ms,first_byte_ms,attempt_count,attribution)
		VALUES($1,$2,$3,'csv-route','chat','openai','caller',$4,$5,200,1200,300,1,'{"session":"sess-42"}'::jsonb)`,
		requestID, uuid.New(), keyID, started, started.Add(1200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	status, headers, body := h.download(owner, "GET", "/api/v1/requests/export.csv", nil)
	if status != 200 {
		t.Fatalf("csv status %d body %s", status, body)
	}
	if got := headers.Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Fatalf("content-type %q", got)
	}
	if cd := headers.Get("Content-Disposition"); !strings.Contains(cd, "olp-requests.csv") {
		t.Fatalf("disposition %q", cd)
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache %q", headers.Get("Cache-Control"))
	}
	if !strings.Contains(body, "\r\n") {
		t.Fatal("no CRLF rows")
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("csv rows %d", len(rows))
	}
	hdr := map[string]int{}
	for i, name := range rows[0] {
		hdr[name] = i
	}
	row := rows[1]
	if row[hdr["request_id"]] != requestID || row[hdr["api_key_id"]] != keyID ||
		row[hdr["route"]] != "csv-route" || row[hdr["session_id"]] != "sess-42" ||
		row[hdr["status_code"]] != "200" || row[hdr["total_latency_ms"]] != "1200" {
		t.Fatalf("row %v", row)
	}
	if row[hdr["estimated_cost"]] != "" && row[hdr["unpriced"]] != "" {
		t.Fatalf("empty cells must be empty not zero: %v", row)
	}
	// project filter
	_, _, filtered := h.download(owner, "GET", "/api/v1/requests/export.csv?project_id="+projectID, nil)
	if len(strings.Split(strings.TrimSpace(filtered), "\r\n")) != 2 {
		t.Fatalf("project filtered rows %q", filtered)
	}
	_, _, foreign := h.download(owner, "GET", "/api/v1/requests/export.csv?project_id="+uuid.NewString(), nil)
	if len(strings.Split(strings.TrimSpace(foreign), "\r\n")) != 1 {
		t.Fatalf("foreign project rows %q", foreign)
	}
	// session filter
	_, _, sess := h.download(owner, "GET", "/api/v1/requests/export.csv?session_id=sess-42", nil)
	if len(strings.Split(strings.TrimSpace(sess), "\r\n")) != 2 {
		t.Fatalf("session rows %q", sess)
	}
	_, _, sessMiss := h.download(owner, "GET", "/api/v1/requests/export.csv?session_id=other", nil)
	if len(strings.Split(strings.TrimSpace(sessMiss), "\r\n")) != 1 {
		t.Fatalf("session miss rows %q", sessMiss)
	}
	status, _, _ = h.request(owner, "GET", "/api/v1/requests/export.csv?session_id=sess-42&attribution_key=region", nil, nil)
	if status != 422 {
		t.Fatalf("conflicting session/attribution = %d, want 422", status)
	}
	// formula-unsafe text stays quoted/neutralised
	injectedID := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count,error_class,attribution)
		VALUES($1,$2,$3,'=cmd|/C','chat','openai','caller',$4,$5,500,1,'rate_limit','{"session":"=2+3"}'::jsonb)`,
		injectedID, uuid.New(), keyID, started.Add(-time.Minute), started); err != nil {
		t.Fatal(err)
	}
	_, _, injected := h.download(owner, "GET", "/api/v1/requests/export.csv", nil)
	if strings.Contains(injected, ",=cmd|/C,") || strings.Contains(injected, ",=2+3") {
		t.Fatalf("formula-unsafe cell unescaped: %q", injected)
	}
	rows, err = csv.NewReader(strings.NewReader(injected)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var injectedRow []string
	for _, row := range rows[1:] {
		if row[hdr["request_id"]] == injectedID {
			injectedRow = row
		}
	}
	if injectedRow == nil || injectedRow[hdr["route"]] != "'=cmd|/C" {
		t.Fatalf("injected row %v", injectedRow)
	}
}

func TestExportRequestCSVScopedCaller(t *testing.T) {
	h := repHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Scope"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	other := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Other"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	otherID := other["id"].(string)
	member := h.invite(owner, "csv-member@example.com", "developer")
	profile := h.want(member, "GET", "/api/v1/profile", nil, nil, 200)
	memberID := profile["id"].(string)
	h.want(owner, "PATCH", "/api/v1/users/"+memberID, map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
	addMember(h, owner, projectID, memberID, "manager")
	member = login(h, "csv-member@example.com")
	key := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "scoped", "scopes": []string{"inference"}, "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	foreign := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "foreign", "scopes": []string{"inference"}, "project_id": otherID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	started := time.Now().Add(-time.Hour)
	for _, k := range []struct{ key, route string }{{key["id"].(string), "scoped-route"}, {foreign["id"].(string), "foreign-route"}} {
		if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count)
			VALUES($1,$2,$3,$4,'chat','openai','caller',$5,$6,200,1)`,
			uuid.New(), uuid.New(), k.key, k.route, started, started.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	_, _, scopedBody := h.download(member, "GET", "/api/v1/requests/export.csv", nil)
	if strings.Contains(scopedBody, "foreign-route") || !strings.Contains(scopedBody, "scoped-route") {
		t.Fatalf("scoped caller rows %q", scopedBody)
	}
	_, _, projectBody := h.download(member, "GET", "/api/v1/requests/export.csv?project_id="+projectID, nil)
	if !strings.Contains(projectBody, "scoped-route") {
		t.Fatalf("project filter rows %q", projectBody)
	}
	status, _, _ := h.request(member, "GET", "/api/v1/requests/export.csv?project_id="+otherID, nil, nil)
	if status != 403 && status != 404 {
		t.Fatalf("foreign project export = %d, want 4xx", status)
	}
	status, _, _ = h.request(member, "GET", "/api/v1/requests/export.csv?project_id="+uuid.NewString(), nil, nil)
	if status != 403 && status != 404 {
		t.Fatalf("unknown project export = %d, want 4xx", status)
	}
}

func TestExportRequestCSVTooLarge(t *testing.T) {
	h := repHarness(t)
	owner := h.owner()
	key := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "bulk", "scopes": []string{"inference"}},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyID := key["id"].(string)
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count)
		SELECT gen_random_uuid(),gen_random_uuid(),$1,'bulk-route','chat','openai','caller',now()-make_interval(secs=>i),now()-make_interval(secs=>i),200,1
		FROM generate_series(1,10001) i`, keyID); err != nil {
		t.Fatal(err)
	}
	status, headers, body := h.download(owner, "GET", "/api/v1/requests/export.csv", nil)
	if status != 422 {
		t.Fatalf("oversize csv = %d body %d bytes", status, len(body))
	}
	if ct := headers.Get("Content-Type"); strings.HasPrefix(ct, "text/csv") {
		t.Fatal("CSV headers committed before the limit check")
	}
}

func TestExportUsageCSVDownload(t *testing.T) {
	f := repSetup(t)
	owner := f.Owner
	now := time.Now().UTC()
	start := now.Add(-6 * time.Hour).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)
	reqID := uuid.NewString()
	f.request(repRequest{ID: reqID, StartedAt: now.Add(-3 * time.Hour), Route: "csv-usage", Operation: "chat", Surface: "openai", StatusCode: ptr(200), AttemptCount: 1, Latency: ptr(500)})
	in, out := int64(10), int64(20)
	cost := "0.000001"
	cur := "USD"
	f.fact(repFact{RequestID: reqID, StartedAt: now.Add(-3 * time.Hour).Add(50 * time.Millisecond), Ordinal: 1, ObservedAt: now.Add(-3 * time.Hour).Add(time.Second),
		Route: "csv-usage", ProviderID: f.P1, Model: "gpt-csv", Operation: "chat", Surface: "openai",
		Observed: true, Complete: true, Charge: "billable", Input: &in, Output: &out, Cost: &cost, Currency: &cur,
		CountRequest: true, CountProvider: true, CountModel: true, CountTarget: true})
	status, headers, body := f.h.download(owner, "GET", "/api/v1/usage/export.csv?start="+start+"&end="+end+"&dimension=route", nil)
	if status != 200 {
		t.Fatalf("usage csv status %d body %s", status, body)
	}
	if !strings.HasPrefix(headers.Get("Content-Type"), "text/csv") || !strings.Contains(headers.Get("Content-Disposition"), "olp-usage.csv") {
		t.Fatalf("headers %v", headers)
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatal("no-store missing")
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatalf("usage csv rows %d", len(rows))
	}
	hdr := map[string]int{}
	for i, name := range rows[0] {
		hdr[name] = i
	}
	var summaryRow, routeRow []string
	for _, row := range rows[1:] {
		switch row[hdr["row_type"]] {
		case "summary":
			summaryRow = row
		case "breakdown":
			routeRow = row
		}
	}
	if summaryRow == nil {
		t.Fatal("no summary row")
	}
	if summaryRow[hdr["request_count"]] != "1" || summaryRow[hdr["input_tokens"]] != "10" || summaryRow[hdr["output_tokens"]] != "20" ||
		summaryRow[hdr["estimated_cost"]] != "0.000001000000" || summaryRow[hdr["currency"]] != "USD" {
		t.Fatalf("summary row %v", summaryRow)
	}
	if routeRow == nil || routeRow[hdr["dimension_value"]] != "csv-usage" {
		t.Fatalf("breakdown rows %v", rows)
	}
}
