//go:build integration

package integration_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/netip"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/tests/harness"
)

type notificationCaller func(method, path, ifMatch, body string, idempotencyKey ...string) apiResult

func TestSavedWebhookTestIsSentOnceAcrossReplicas(t *testing.T) {
	for _, ending := range []string{"acknowledged", "timeout", "interrupted"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			exerciseWebhookTest(t, ending)
		})
	}
}

func exerciseWebhookTest(t *testing.T, ending string) {
	t.Helper()
	pool, fixture, first, second := testNotificationReplicas(t)
	type received struct{ body, signature string }
	messages := make(chan received, 8)
	ack := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		messages <- received{string(body), r.Header.Get(egress.SignatureHeader)}
		select {
		case <-ack:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(receiver.Close)
	const signingKey = "fixture-signing-key"
	destination := saveTestDestination(t, first, fmt.Sprintf(`{"name":"test webhook","url":%q,"project_id":%q,"secret":%q}`, receiver.URL, fixture.projectID, signingKey))
	rule := seedTestIncident(t, pool, fixture, destination)
	before := testIncidentState(t, pool, rule)
	path := newNotificationTestPath(destination)
	original := make(chan apiResult, 1)
	go func() { original <- first("POST", path, "", "") }()

	checkWebhook := func() {
		t.Helper()
		got := waitTestMessage(t, messages)
		assertMarkedTest(t, []byte(got.body))
		expected := make(http.Header)
		egress.SignBody(expected, []byte(signingKey), []byte(got.body))
		if got.signature != expected.Get(egress.SignatureHeader) {
			t.Fatal("test webhook was not signed with the saved key")
		}
	}
	checkWebhook()
	pending := requireTestStatus(t, second("POST", path, "", ""), "pending")
	if ending == "interrupted" {
		if _, err := pool.Exec(t.Context(), `UPDATE olp.notification_destination_tests SET expires_at=now()-interval '1 second' WHERE id=$1`, pending.Id); err != nil {
			t.Fatal(err)
		}
		requireTestStatus(t, second("GET", path, "", ""), "unconfirmed")
	}
	if ending != "timeout" {
		close(ack)
	}
	want := "delivered"
	if ending != "acknowledged" {
		want = "unconfirmed"
	}
	got := requireTestStatus(t, waitTestMessage(t, original), want)
	if ending == "timeout" {
		close(ack)
		if got.Failure == nil || *got.Failure != "timeout" {
			t.Fatalf("expected sanitized timeout code, got %+v", got)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		replayed := requireTestStatus(t, second(method, path, "", ""), want)
		if replayed.Id != pending.Id || replayed.Id != got.Id {
			t.Fatalf("%s returned ID %q, original and concurrent IDs %q and %q", method, replayed.Id, got.Id, pending.Id)
		}
	}
	assertTestAudit(t, pool, fixture.projectID, got.Id.String(), want, ending == "interrupted")
	if len(messages) != 0 {
		t.Fatalf("replays sent %d extra webhooks", len(messages))
	}
	for range 2 {
		newResult := requireTestStatus(t, first("POST", newNotificationTestPath(destination), "", ""), "delivered")
		if newResult.Id == got.Id {
			t.Fatal("a separate action reused the previous test record")
		}
		checkWebhook()
	}
	if limited := first("POST", newNotificationTestPath(destination), "", ""); limited.status != http.StatusTooManyRequests {
		t.Fatalf("fourth new test returned %d %s, want 429", limited.status, limited.body)
	}
	requireTestStatus(t, second("POST", path, "", ""), want)
	if len(messages) != 0 || testIncidentState(t, pool, rule) != before {
		t.Fatal("rate-limited or replayed tests sent a webhook, or tests changed incident or retry state")
	}
}

func TestSavedEmailTestIsSentOnceAcrossReplicas(t *testing.T) {
	t.Parallel()
	_, _, first, second := testNotificationReplicas(t)
	url, certificate, messages, ack := openTestSMTPSink(t)
	destination := saveTestDestination(t, first, fmt.Sprintf(`{"name":"test email","url":%q,"type":"email","configuration":{"from":"alerts@example.invalid","to":["ops@example.invalid"],"ca_certificate":%q}}`, url, certificate))
	path := newNotificationTestPath(destination)
	original := make(chan apiResult, 1)
	go func() { original <- first("POST", path, "", "") }()
	raw := waitTestMessage(t, messages)
	pending := requireTestStatus(t, second("POST", path, "", ""), "pending")
	close(ack)
	sent := requireTestStatus(t, waitTestMessage(t, original), "delivered")
	replayed := requireTestStatus(t, second("POST", path, "", ""), "delivered")
	if pending.Id != sent.Id || replayed.Id != sent.Id || len(messages) != 0 {
		t.Fatal("email replay did not return the original result or the sink received another message")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Header.Get("Subject"), "OLP test notification") || !strings.Contains(parsed.Header.Get("To"), "ops@example.invalid") {
		t.Fatalf("test mail did not have the test subject and saved recipient: %v", parsed.Header)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(parsed.Body))
	if err != nil {
		t.Fatal(err)
	}
	assertMarkedTest(t, body)
}

func testNotificationReplicas(t *testing.T) (*pgxpool.Pool, usageFixture, notificationCaller, notificationCaller) {
	t.Helper()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	return pool, fixture, notificationManagementWithPolicy(t, pool, fixture.ownerID, policy), notificationManagementWithPolicy(t, pool, fixture.ownerID, policy)
}

func TestSavedDestinationTestRequiresWriteScopeAndEnabledDestination(t *testing.T) {
	t.Parallel()
	pool, f, owner, _ := testNotificationReplicas(t)
	id := saveTestDestination(t, owner, fmt.Sprintf(`{"name":"disabled webhook","url":"https://example.test","project_id":%q,"enabled":false}`, f.projectID))
	user := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag,access_scope)
 VALUES($1,$2,'Project viewer','operator',$1,'assigned')`, user, user+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	member := notificationManagementAs(t, pool, user)
	path := newNotificationTestPath(id)
	for _, method := range []string{"GET", "POST"} {
		if reply := member(method, path, "", ""); reply.status != http.StatusNotFound {
			t.Fatalf("out-of-scope %s returned %d %s, want 404", method, reply.status, reply.body)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.project_members(project_id,user_id,role,added_by)
 VALUES($1,$2,'viewer',$3)`, f.projectID, user, f.ownerID); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		if reply := member(method, path, "", ""); reply.status != http.StatusForbidden {
			t.Fatalf("project viewer %s returned %d %s, want 403", method, reply.status, reply.body)
		}
	}
	if reply := owner("POST", path, "", ""); reply.status != http.StatusUnprocessableEntity {
		t.Fatalf("disabled destination returned %d %s, want 422", reply.status, reply.body)
	}
	if reply := owner("GET", path, "", ""); reply.status != http.StatusNotFound {
		t.Fatalf("rejected test left a record: %d %s", reply.status, reply.body)
	}
}

func saveTestDestination(t *testing.T, call notificationCaller, body string) string {
	t.Helper()
	saved := call("POST", "/api/v1/notifications/destinations", "", body)
	var destination struct{ ID string }
	if err := json.Unmarshal(saved.body, &destination); err != nil || saved.status != http.StatusCreated || destination.ID == "" {
		t.Fatalf("save destination returned %d %s (%v)", saved.status, saved.body, err)
	}
	return destination.ID
}

func newNotificationTestPath(destination string) string {
	return "/api/v1/notifications/destinations/" + destination + "/tests/" + uuid.NewString()
}

func requireTestStatus(t *testing.T, response apiResult, status string) contract.NotificationDestinationTest {
	t.Helper()
	var test contract.NotificationDestinationTest
	if err := json.Unmarshal(response.body, &test); err != nil || response.status != http.StatusOK || string(test.Status) != status || test.Id == uuid.Nil {
		t.Fatalf("test returned %d %s (%v), want %q", response.status, response.body, err, status)
	}
	return test
}

func waitTestMessage[T any](t *testing.T, messages <-chan T) T {
	t.Helper()
	select {
	case msg := <-messages:
		return msg
	case <-time.After(7 * time.Second):
		t.Fatal("timed out waiting for notification test")
		var zero T
		return zero
	}
}

func assertMarkedTest(t *testing.T, body []byte) {
	t.Helper()
	var payload struct {
		Event string
		Test  bool
	}
	if err := json.Unmarshal(body, &payload); err != nil || !payload.Test || payload.Event != "notification.test" {
		t.Fatalf("message not marked as a test: %q (%v)", body, err)
	}
}

func seedTestIncident(t *testing.T, pool *pgxpool.Pool, f usageFixture, destination string) string {
	t.Helper()
	rule := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,destination_id,etag,created_by)
 VALUES($1,'existing rule',$2,'key.expiring','api_key',$3,$4,$1,$5)`, rule, f.projectID, f.keyID, destination, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status,attempts,last_attempt_at)
 VALUES($1,$2,'key.expiring','existing',false,'{}','failed',1,now())`, uuid.NewString(), rule); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_signal_states(rule_id,subject,active,incident,last_trigger_at)
 VALUES($1,'existing',true,1,now())`, rule); err != nil {
		t.Fatal(err)
	}
	return rule
}

func testIncidentState(t *testing.T, pool *pgxpool.Pool, rule string) string {
	t.Helper()
	var state string
	err := pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(r),
 (SELECT jsonb_agg(to_jsonb(d) ORDER BY d.id) FROM olp.notification_deliveries d WHERE d.rule_id=r.id),
 (SELECT jsonb_agg(to_jsonb(s) ORDER BY s.subject) FROM olp.notification_signal_states s WHERE s.rule_id=r.id))::text
 FROM olp.notification_rules r WHERE r.id=$1`, rule).Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertTestAudit(t *testing.T, pool *pgxpool.Pool, projectID, testID, status string, interrupted bool) {
	t.Helper()
	var actions []string
	err := pool.QueryRow(t.Context(), `SELECT array_agg(action ORDER BY action) FROM olp.audit
 WHERE project_id=$1 AND resource_id=$2 AND resource_type='notification_destination_test'`, projectID, testID).Scan(&actions)
	want := []string{"notification_destination_test.claim"}
	if !interrupted {
		want = append(want, "notification_destination_test."+status)
	}
	if err != nil || strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("test audit actions %v (%v), want %v", actions, err, want)
	}
}

func openTestSMTPSink(t *testing.T) (string, string, <-chan []byte, chan struct{}) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw})
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", fixture.TLS.Clone())
	fixture.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	messages := make(chan []byte, 8)
	ack := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSMTP(conn, messages, ack)
		}
	}()
	return "smtps://" + listener.Addr().String(), string(certificate), messages, ack
}

func serveTestSMTP(socket net.Conn, messages chan<- []byte, ack <-chan struct{}) {
	conn := textproto.NewConn(socket)
	defer conn.Close()
	_ = socket.SetDeadline(time.Now().Add(8 * time.Second))
	_ = conn.PrintfLine("220 local SMTP sink")
	for {
		line, err := conn.ReadLine()
		if err != nil {
			return
		}
		switch cmd, _, _ := strings.Cut(line, " "); cmd {
		case "EHLO", "MAIL", "RCPT":
			_ = conn.PrintfLine("250 OK")
		case "DATA":
			_ = conn.PrintfLine("354 Send data")
			body, err := conn.ReadDotBytes()
			if err != nil {
				return
			}
			messages <- body
			select {
			case <-ack:
				_ = conn.PrintfLine("250 Accepted")
			case <-time.After(8 * time.Second):
				return
			}
		case "QUIT":
			_ = conn.PrintfLine("221 Bye")
			return
		default:
			_ = conn.PrintfLine("500 Unsupported command")
		}
	}
}
