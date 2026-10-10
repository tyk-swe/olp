//go:build integration

package integration_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type channelPosts struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func (c *channelPosts) add(path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, string(body))
	c.paths = append(c.paths, path)
}

func (c *channelPosts) snapshot() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...), append([]string(nil), c.paths...)
}

func notificationCA(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "notify CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "smtp.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustECKey(t, srvKey)}))
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), cert
}

func mustECKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func smtpFixture(t *testing.T, cert *tls.Certificate, starttls bool) (addr string, messages chan string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	messages = make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSMTP(conn, cert, !starttls, starttls, messages)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String(), messages
}

func serveSMTP(conn net.Conn, cert *tls.Certificate, implicit, starttls bool, messages chan string) {
	defer conn.Close()
	if implicit {
		conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	}
	reader := bufio.NewReader(conn)
	write := func(s string) { _, _ = conn.Write([]byte(s)) }
	write("220 notify.test ESMTP\r\n")
	var data strings.Builder
	inData := false
	for {
		line, err := reader.ReadString(0x0a)
		if err != nil {
			return
		}
		if inData {
			data.WriteString(line)
			if strings.TrimSpace(line) == "." {
				inData = false
				write("250 queued\r\n")
				select {
				case messages <- data.String():
				default:
				}
			}
			continue
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if starttls {
				write("250-notify.test\r\n250-STARTTLS\r\n250-AUTH PLAIN\r\n250 OK\r\n")
			} else {
				write("250 notify.test\r\n")
			}
		case command == "STARTTLS":
			write("220 Go ahead\r\n")
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
			reader = bufio.NewReader(conn)
		case strings.HasPrefix(command, "AUTH"):
			write("235 authenticated\r\n")
		case strings.HasPrefix(command, "MAIL"), strings.HasPrefix(command, "RCPT"):
			write("250 ok\r\n")
		case command == "DATA":
			write("354 end with .\r\n")
			inData = true
		case command == "QUIT":
			write("221 bye\r\n")
			return
		default:
			write("502 unsupported\r\n")
		}
	}
}

func TestNotificationEndpointChangesRequireExplicitSecrets(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for _, kind := range []string{"pagerduty", "email"} {
		t.Run(kind, func(t *testing.T) {
			endpoint, next := "https://events.example/enqueue", "https://other.example/enqueue"
			secret := map[string]any{"routing_key": "private-routing-key"}
			configuration := map[string]any{}
			if kind == "email" {
				endpoint, next = "smtps://mail.example:465", "smtps://other.example:465"
				secret = map[string]any{"username": "operator", "password": "private-password"}
				configuration = map[string]any{"from": "alerts@example.com", "to": []string{"ops@example.com"}}
			}
			destination := h.want(owner, "POST", "/api/v1/notifications/destinations", map[string]any{
				"name": kind, "type": kind, "url": endpoint, "secret": secret, "configuration": configuration,
			}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			path := "/api/v1/notifications/destinations/" + destination["id"].(string)
			h.want(owner, "PATCH", path, map[string]any{"url": next}, etagHeader(destination), 422)
			unchanged := h.want(owner, "GET", path, nil, nil, 200)
			if unchanged["url"] != endpoint || unchanged["etag"] != destination["etag"] {
				t.Fatal("rejected update changed the secret destination")
			}
			destination = h.want(owner, "PATCH", path, map[string]any{"url": next, "secret": secret}, etagHeader(destination), 200)
			destination = h.want(owner, "PATCH", path, map[string]any{"url": endpoint, "enabled": false, "secret": nil}, etagHeader(destination), 200)
			if destination["secret_configured"] != false {
				t.Fatal("explicit removal retained the old secret")
			}
		})
	}
}

func TestNotificationChannelManagementAndDelivery(t *testing.T) {
	h := newAccessHarness(t)
	h.Server.Egress = alertPolicy()
	owner := h.owner()

	posts := map[string]*channelPosts{}
	receiver := func(name string) *httptest.Server {
		posts[name] = &channelPosts{}
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			posts[name].add(r.URL.Path, buf)
			if name == "pagerduty" {
				var decoded map[string]any
				_ = json.Unmarshal(buf, &decoded)
				dedup, _ := decoded["dedup_key"].(string)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"success","dedup_key":"` + dedup + `"}`))
				return
			}
			w.WriteHeader(204)
		}))
	}
	webhookServer := receiver("webhook")
	slackServer := receiver("slack")
	teamsServer := receiver("msteams")
	discordServer := receiver("discord")
	pdServer := receiver("pagerduty")
	defer webhookServer.Close()
	defer slackServer.Close()
	defer teamsServer.Close()
	defer discordServer.Close()
	defer pdServer.Close()

	caPEM, cert := notificationCA(t)
	smtpAddr, smtpMessages := smtpFixture(t, &cert, false)

	hookSecret := func(server *httptest.Server) map[string]any {
		return map[string]any{"webhook_url": server.URL + "/hook/tokenized?key=x"}
	}
	destinations := map[string]map[string]any{
		"webhook":   {"name": "wh", "url": webhookServer.URL, "secret": "sig-secret"},
		"slack":     {"name": "sl", "type": "slack", "url": slackServer.URL, "secret": hookSecret(slackServer)},
		"msteams":   {"name": "tm", "type": "msteams", "url": teamsServer.URL, "secret": hookSecret(teamsServer)},
		"discord":   {"name": "dc", "type": "discord", "url": discordServer.URL, "secret": hookSecret(discordServer)},
		"pagerduty": {"name": "pd", "type": "pagerduty", "url": pdServer.URL + "/enqueue", "secret": map[string]any{"routing_key": "rk-secret"}},
		"email": {"name": "em", "type": "email", "url": "smtps://" + smtpAddr,
			"configuration": map[string]any{"from": "alerts@olp.test", "to": []string{"ops@example.test"}, "ca_certificate": caPEM}},
	}
	var ownerID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.users WHERE email='owner@example.com'").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for typ, body := range destinations {
		created := h.want(owner, "POST", "/api/v1/notifications/destinations", body,
			map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
		ids[typ] = created["id"].(string)
		if typ == "webhook" && created["type"] != "webhook" {
			t.Fatalf("webhook type %v", created["type"])
		}
		if typ != "webhook" && created["type"] != typ {
			t.Fatalf("%s type %v", typ, created["type"])
		}
		serialized, _ := json.Marshal(created)
		for _, raw := range []string{"webhook_url", "routing_key", "password", "sig-secret", "rk-secret"} {
			if strings.Contains(string(serialized), raw) {
				t.Fatalf("%s destination response contains secret %q", typ, raw)
			}
		}
		wantSecret := typ != "email"
		if created["secret_configured"] != wantSecret {
			t.Fatalf("%s secret_configured %v", typ, created["secret_configured"])
		}
	}

	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/destinations", map[string]any{
		"name": "bad", "type": "slack", "url": "https://hooks.example.com/path",
		"secret": hookSecret(slackServer)}, map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("slack origin-with-path = %d, want 422", status)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/destinations", map[string]any{
		"name": "bad2", "type": "pagerduty", "url": pdServer.URL}, map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("pagerduty without routing_key = %d, want 422", status)
	}

	rules := map[string]string{}
	for typ, id := range ids {
		created := h.want(owner, "POST", "/api/v1/notifications/rules", map[string]any{
			"name": typ + "-rule", "event": "worker.stale", "destination_id": id,
			"configuration": map[string]any{"cooldown_seconds": 120},
		}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
		rules[typ] = created["id"].(string)
		if created["configuration"].(map[string]any)["cooldown_seconds"] != float64(120) {
			t.Fatalf("%s rule configuration %v", typ, created["configuration"])
		}
	}

	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Notify"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	projectDestination := h.want(owner, "POST", "/api/v1/notifications/destinations",
		map[string]any{"name": "scoped wh", "url": webhookServer.URL + "/p", "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "bad3", "event": "worker.stale", "destination_id": projectDestination["id"], "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("worker.stale project rule = %d, want 422", status)
	}
	projectRule := h.want(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "scoped latency", "event": "route.latency", "destination_id": projectDestination["id"], "project_id": projectID,
		"configuration": map[string]any{"threshold": "500", "metric": "ttft"}},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if projectRule["configuration"].(map[string]any)["metric"] != "ttft" {
		t.Fatalf("project rule configuration %v", projectRule["configuration"])
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "crossed", "event": "route.latency", "destination_id": ids["webhook"], "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("foreign destination rule = %d, want 422", status)
	}
	viewer := h.invite(owner, "viewer@example.com", "viewer")
	if status, _, _ := h.request(viewer, "POST", "/api/v1/notifications/destinations",
		map[string]any{"name": "foreign", "url": webhookServer.URL + "/v", "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 403 {
		t.Fatalf("viewer project destination = %d, want 403", status)
	}
	if status, _, _ := h.request(viewer, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "foreign rule", "event": "report.spend", "destination_id": projectDestination["id"], "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 403 {
		t.Fatalf("viewer project rule = %d, want 403", status)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "bad4", "event": "route.latency", "destination_id": ids["webhook"], "subject_id": uuid.NewString()},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("generic event with subject = %d, want 422", status)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "bad5", "event": "route.latency", "destination_id": ids["webhook"],
		"configuration": map[string]any{"threshold": "100", "unknown_field": 1}},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("unknown configuration field = %d, want 422", status)
	}

	slackDetail := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200)
	patched := h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"type": "slack"}, withMatch(slackDetail, nil), 200)
	if patched["type"] != "slack" {
		t.Fatalf("patched type %v", patched["type"])
	}
	latestSlack := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200)
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"type": "discord"}, withMatch(latestSlack, nil)); status != 422 {
		t.Fatalf("type change = %d, want 422", status)
	}
	emailDetail := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["email"], nil, nil, 200)
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["email"],
		map[string]any{"configuration": nil}, withMatch(emailDetail, nil)); status >= 400 {
		t.Logf("email configuration null refused: %d", status)
	} else {
		t.Fatal("email configuration cleared")
	}
	webhookDetail := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["webhook"], nil, nil, 200)
	webhookPatched := h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["webhook"],
		map[string]any{"configuration": nil}, withMatch(webhookDetail, nil), 200)
	if cfg, ok := webhookPatched["configuration"].(map[string]any); !ok || len(cfg) != 0 {
		t.Fatalf("webhook configuration %v", webhookPatched["configuration"])
	}

	secretState := func(destination string) (*string, int) {
		var current *string
		var stored int
		if err := h.Pool.QueryRow(t.Context(), `SELECT d.secret_id::text,
		    (SELECT count(*) FROM olp.secrets WHERE purpose='notification_secret')
		    FROM olp.notification_destinations d WHERE d.id=$1`, destination).Scan(&current, &stored); err != nil {
			t.Fatal(err)
		}
		return current, stored
	}
	before, storedBefore := secretState(ids["slack"])
	if before == nil {
		t.Fatal("slack secret not sealed")
	}
	freshDetail := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"secret": hookSecret(slackServer)}, withMatch(freshDetail, nil), 200)
	after, storedAfter := secretState(ids["slack"])
	if after == nil || *after == *before || storedAfter != storedBefore {
		t.Fatalf("rotated object secret = %v, %d stored", after, storedAfter)
	}
	secondHook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(secondHook.Close)
	foreignOrigin := secondHook.URL
	freshSlack := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200)
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"url": foreignOrigin}, withMatch(freshSlack, nil)); status != 422 {
		t.Fatalf("origin change without matching secret = %d, want 422", status)
	}
	secretBefore, _ := secretState(ids["slack"])
	patchedURL := h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"url": foreignOrigin, "secret": map[string]any{"webhook_url": foreignOrigin + "/moved"}},
		withMatch(h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200), nil), 200)
	if patchedURL["url"] != foreignOrigin {
		t.Fatalf("url after allowed move %v", patchedURL["url"])
	}
	secretMoved, _ := secretState(ids["slack"])
	if secretMoved == nil || secretBefore == nil || *secretMoved == *secretBefore {
		t.Fatal("moved webhook_url did not rotate the secret")
	}
	latest := h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"name": "slack-renamed"}, withMatch(latest, nil), 200)
	secretKeep, _ := secretState(ids["slack"])
	if secretKeep == nil || *secretKeep != *secretMoved {
		t.Fatal("omitted secret changed the stored credential")
	}
	disabled := h.want(owner, "POST", "/api/v1/notifications/destinations",
		map[string]any{"name": "slack-disabled", "type": "slack", "url": slackServer.URL, "enabled": false},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if disabled["secret_configured"] != false {
		t.Fatalf("disabled create secret_configured %v", disabled["secret_configured"])
	}
	disabledID := disabled["id"].(string)
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/notifications/destinations/"+disabledID,
		map[string]any{"enabled": true},
		withMatch(h.want(owner, "GET", "/api/v1/notifications/destinations/"+disabledID, nil, nil, 200), nil)); status != 422 {
		t.Fatalf("enable without secret = %d, want 422", status)
	}
	h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+disabledID,
		map[string]any{"enabled": true, "secret": hookSecret(slackServer)},
		withMatch(h.want(owner, "GET", "/api/v1/notifications/destinations/"+disabledID, nil, nil, 200), nil), 200)
	createdSecret, _ := secretState(disabledID)
	if createdSecret == nil {
		t.Fatal("rotate+enable did not store the secret")
	}
	h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+disabledID,
		map[string]any{"enabled": false, "secret": nil},
		withMatch(h.want(owner, "GET", "/api/v1/notifications/destinations/"+disabledID, nil, nil, 200), nil), 200)
	cleared, _ := secretState(disabledID)
	if cleared != nil {
		t.Fatal("secret null while disabled did not clear the credential")
	}
	gone := h.want(owner, "GET", "/api/v1/notifications/destinations/"+disabledID, nil, nil, 200)
	if gone["secret_configured"] != false {
		t.Fatalf("cleared secret still configured: %v", gone["secret_configured"])
	}
	h.want(owner, "PATCH", "/api/v1/notifications/destinations/"+ids["slack"],
		map[string]any{"url": slackServer.URL, "secret": hookSecret(slackServer)},
		withMatch(h.want(owner, "GET", "/api/v1/notifications/destinations/"+ids["slack"], nil, nil, 200), nil), 200)

	insert := func(ruleID, event, dedup string, resolved bool, payload map[string]any) string {
		t.Helper()
		id := uuid.NewString()
		payload["version"], payload["delivery_id"], payload["event"], payload["rule_id"] = 1, id, event, ruleID
		payload["incident_key"], payload["dedup_key"], payload["resolved"] = dedup, dedup, resolved
		raw, _ := json.Marshal(payload)
		if _, err := h.Pool.Exec(t.Context(),
			`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
			 VALUES($1,$2,$3,$4,$5,$6,'pending')`, id, ruleID, event, dedup, resolved, raw); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,event,destination_id,etag,created_by,enabled)
		SELECT gen_random_uuid(),'filler','worker.stale',$1,gen_random_uuid(),$2,false
		FROM generate_series(1,993)`, ids["webhook"], ownerID); err != nil {
		t.Fatalf("cap fillers: %v", err)
	}
	var genericCount int
	if err := h.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM olp.notification_rules WHERE event NOT IN ('budget.threshold','provider.grant.lapsed','key.expiring')`).Scan(&genericCount); err != nil {
		t.Fatal(err)
	}
	if genericCount != 1000 {
		t.Fatalf("generic rules = %d, want 1000", genericCount)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "overflow", "event": "worker.stale", "destination_id": ids["webhook"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}); status >= 400 {
		t.Logf("rule over cap refused: %d", status)
	} else {
		t.Fatal("1001st generic rule accepted")
	}

	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.worker_task_health(task,checked_at,last_success_at,first_seen_at,successes_total)
		 VALUES('media_reconciliation',now()-interval '10 minutes',NULL,now()-interval '10 minutes',0)`); err != nil {
		t.Fatal(err)
	}
	deliveryPass(t, h, alertPolicy())

	for _, typ := range []string{"webhook", "slack", "msteams", "discord", "pagerduty"} {
		bodies, _ := posts[typ].snapshot()
		if len(bodies) != 1 {
			t.Fatalf("%s posts = %d, want 1", typ, len(bodies))
		}
	}
	slackBody, _ := posts["slack"].snapshot()
	var slack map[string]any
	if err := json.Unmarshal([]byte(slackBody[0]), &slack); err != nil || slack["blocks"] == nil {
		t.Fatalf("slack body %s", slackBody[0])
	}
	teamsBody, _ := posts["msteams"].snapshot()
	var teams map[string]any
	if err := json.Unmarshal([]byte(teamsBody[0]), &teams); err != nil ||
		teams["attachments"].([]any)[0].(map[string]any)["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("teams body %s", teamsBody[0])
	}
	discordBody, _ := posts["discord"].snapshot()
	var discord map[string]any
	if err := json.Unmarshal([]byte(discordBody[0]), &discord); err != nil || discord["embeds"] == nil {
		t.Fatalf("discord body %s", discordBody[0])
	}
	pdBody, _ := posts["pagerduty"].snapshot()
	var pd map[string]any
	if err := json.Unmarshal([]byte(pdBody[0]), &pd); err != nil ||
		pd["routing_key"] != "rk-secret" || pd["event_action"] != "trigger" {
		t.Fatalf("pagerduty body %s", pdBody[0])
	}
	whBody, _ := posts["webhook"].snapshot()
	var wh map[string]any
	if err := json.Unmarshal([]byte(whBody[0]), &wh); err != nil || wh["event"] != "worker.stale" {
		t.Fatalf("webhook body %s", whBody[0])
	}
	select {
	case message := <-smtpMessages:
		if !strings.Contains(message, "Subject:") || !strings.Contains(message, "alerts@olp.test") {
			t.Fatalf("smtp message %q", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no smtp message")
	}

	if _, err := h.Pool.Exec(t.Context(),
		`UPDATE olp.worker_task_health SET last_success_at=now(),checked_at=now() WHERE task='media_reconciliation'`); err != nil {
		t.Fatal(err)
	}
	deliveryPass(t, h, alertPolicy())
	deliveryPass(t, h, alertPolicy())
	pdBodies, _ := posts["pagerduty"].snapshot()
	if len(pdBodies) != 2 {
		t.Fatalf("pagerduty posts = %d, want trigger+resolve", len(pdBodies))
	}
	var trigger map[string]any
	if err := json.Unmarshal([]byte(pdBodies[0]), &trigger); err != nil ||
		trigger["event_action"] != "trigger" {
		t.Fatalf("pagerduty trigger %s", pdBodies[0])
	}
	var resolve map[string]any
	if err := json.Unmarshal([]byte(pdBodies[1]), &resolve); err != nil ||
		resolve["event_action"] != "resolve" || resolve["dedup_key"] != trigger["dedup_key"] {
		t.Fatalf("pagerduty resolve %s", pdBodies[1])
	}

	staleID := insert(rules["webhook"], "worker.stale", "olp:notify:stale:1", false,
		map[string]any{"subject": "media_reconciliation", "incident": 99})
	deliveryPass(t, h, alertPolicy())
	var status string
	if err := h.Pool.QueryRow(t.Context(),
		"SELECT status FROM olp.notification_deliveries WHERE id=$1", staleID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("stale trigger status %q, want cancelled", status)
	}
	whBodies, _ := posts["webhook"].snapshot()
	for _, raw := range whBodies {
		var delivered map[string]any
		if err := json.Unmarshal([]byte(raw), &delivered); err == nil && delivered["dedup_key"] == "olp:notify:stale:1" {
			t.Fatalf("superseded trigger delivered: %s", raw)
		}
	}
}

func TestNotificationRulesEveryEventAndDispatch(t *testing.T) {
	h := newAccessHarness(t)
	h.Server.Egress = alertPolicy()
	owner := h.owner()
	posts := &channelPosts{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		posts.add(r.URL.Path, buf)
		w.WriteHeader(204)
	}))
	t.Cleanup(hook.Close)

	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Events"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	key := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "alert key", "scopes": []string{"inference"}, "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyID := key["id"].(string)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.api_keys SET expires_at=now()+interval '1 day' WHERE id=$1", keyID); err != nil {
		t.Fatal(err)
	}
	var expiresAt time.Time
	if err := h.Pool.QueryRow(t.Context(), "SELECT expires_at FROM olp.api_keys WHERE id=$1", keyID).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	projectDest := h.want(owner, "POST", "/api/v1/notifications/destinations",
		map[string]any{"name": "scoped", "url": hook.URL + "/scoped", "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	globalDest := h.want(owner, "POST", "/api/v1/notifications/destinations",
		map[string]any{"name": "global", "url": hook.URL + "/global"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	type ruleSpec struct {
		project *string
		dest    string
		body    map[string]any
	}
	specs := map[string]ruleSpec{
		"budget.threshold":            {&projectID, projectDest["id"].(string), map[string]any{"subject_kind": "api_key", "subject_id": keyID, "window_kind": "day", "threshold_percent": 50}},
		"key.expiring":                {&projectID, projectDest["id"].(string), map[string]any{"subject_kind": "api_key", "subject_id": keyID}},
		"provider.grant.lapsed":       {nil, globalDest["id"].(string), nil},
		"budget.exhausted":            {&projectID, projectDest["id"].(string), map[string]any{"configuration": map[string]any{"cooldown_seconds": 120}}},
		"provider.circuit.open":       {nil, globalDest["id"].(string), map[string]any{"configuration": map[string]any{"cooldown_seconds": 120}}},
		"provider.circuit.closed":     {nil, globalDest["id"].(string), map[string]any{"configuration": map[string]any{"cooldown_seconds": 60}}},
		"provider.error_rate":         {nil, globalDest["id"].(string), map[string]any{"configuration": map[string]any{"threshold": "0.5", "window_seconds": 300}}},
		"route.latency":               {&projectID, projectDest["id"].(string), map[string]any{"configuration": map[string]any{"threshold": "800", "metric": "ttft"}}},
		"provider.credential.failing": {nil, globalDest["id"].(string), map[string]any{"configuration": map[string]any{"threshold": "2"}}},
		"model.retirement":            {&projectID, projectDest["id"].(string), map[string]any{"configuration": map[string]any{"lead_days": 10}}},
		"runtime.install_failed":      {nil, globalDest["id"].(string), map[string]any{"configuration": map[string]any{"cooldown_seconds": 60}}},
		"worker.stale":                {nil, globalDest["id"].(string), map[string]any{"configuration": map[string]any{"cooldown_seconds": 60}}},
		"report.spend":                {&projectID, projectDest["id"].(string), map[string]any{"configuration": map[string]any{"period": "weekly"}}},
	}
	rules := map[string]string{}
	for event, spec := range specs {
		body := map[string]any{"name": "rule-" + event, "event": event, "destination_id": spec.dest}
		if spec.project != nil {
			body["project_id"] = *spec.project
		}
		for k, v := range spec.body {
			body[k] = v
		}
		created := h.want(owner, "POST", "/api/v1/notifications/rules", body,
			map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
		if created["event"] != event {
			t.Fatalf("%s event %v", event, created["event"])
		}
		cfg, ok := created["configuration"].(map[string]any)
		if !ok {
			t.Fatalf("%s configuration %v", event, created["configuration"])
		}
		if len(cfg) == 0 && event != "budget.threshold" && event != "provider.grant.lapsed" {
			t.Fatalf("%s canonical configuration is empty: %v", event, created["configuration"])
		}
		rules[event] = created["id"].(string)
	}

	insertDelivery := func(ruleID, event, dedup string, resolved bool, payload map[string]any) {
		t.Helper()
		id := uuid.NewString()
		payload["version"], payload["delivery_id"], payload["event"], payload["rule_id"] = 1, id, event, ruleID
		payload["incident_key"], payload["dedup_key"], payload["resolved"] = dedup, dedup, resolved
		raw, _ := json.Marshal(payload)
		if _, err := h.Pool.Exec(t.Context(),
			`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
			 VALUES($1,$2,$3,$4,$5,$6,'pending')`, id, ruleID, event, dedup, resolved, raw); err != nil {
			t.Fatal(err)
		}
	}
	for event, rule := range rules {
		switch event {
		case "budget.threshold":
			if _, err := h.Pool.Exec(t.Context(),
				`INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status)
				 VALUES($1,$2,7,50,'8','10','pending')`, uuid.NewString(), rule); err != nil {
				t.Fatal(err)
			}
		case "key.expiring":
			if _, err := h.Pool.Exec(t.Context(),
				`INSERT INTO olp.notification_deliveries(id,rule_id,api_key_id,due_at,reason,payload,status)
				 VALUES($1,$2,$3,$4,'expiry',$5,'pending')`, uuid.NewString(), rule, keyID, expiresAt,
				[]byte(`{"api_key_id":"`+keyID+`","api_key_name":"alert key","project_id":"`+projectID+`"}`)); err != nil {
				t.Fatal(err)
			}
		default:
			subject := "subject:" + event
			if _, err := h.Pool.Exec(t.Context(),
				`INSERT INTO olp.notification_signal_states(rule_id,subject,active,incident) VALUES($1,$2,true,1)
				 ON CONFLICT (rule_id,subject) DO UPDATE SET active=true,incident=1`, rule, subject); err != nil {
				t.Fatal(err)
			}
			insertDelivery(rule, event, "olp:evt:"+event+":1", false,
				map[string]any{"subject": subject, "incident": 1, "evidence": map[string]any{"note": "test"}})
		}
	}
	deliveryPass(t, h, alertPolicy())
	deliveryPass(t, h, alertPolicy())

	bodies, _ := posts.snapshot()
	seen := map[string]int{}
	for _, raw := range bodies {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("delivered body %s", raw)
		}
		event, _ := decoded["event"].(string)
		seen[event]++
	}
	for event := range specs {
		if seen[event] != 1 {
			t.Fatalf("event %s delivered %d times", event, seen[event])
		}
	}
}

// Every finalized event is delivered through every channel type by the real
// worker against local fake receivers — HTTP channels post transformed bodies,
// PagerDuty expects an acknowledged event, email lands in the fake SMTP box.
// Project-scoped events go to project-scoped destinations of the same type.
func TestNotificationEveryEventEveryChannel(t *testing.T) {
	h := newAccessHarness(t)
	h.Server.Egress = alertPolicy()
	owner := h.owner()

	posts := map[string]*channelPosts{}
	receiver := func(name string) *httptest.Server {
		posts[name] = &channelPosts{}
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			posts[name].add(r.URL.Path, buf)
			if name == "pagerduty" {
				var decoded map[string]any
				_ = json.Unmarshal(buf, &decoded)
				dedup, _ := decoded["dedup_key"].(string)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"success","dedup_key":"` + dedup + `"}`))
				return
			}
			w.WriteHeader(204)
		}))
	}
	webhookServer := receiver("webhook")
	slackServer := receiver("slack")
	teamsServer := receiver("msteams")
	discordServer := receiver("discord")
	pdServer := receiver("pagerduty")
	defer webhookServer.Close()
	defer slackServer.Close()
	defer teamsServer.Close()
	defer discordServer.Close()
	defer pdServer.Close()

	caPEM, cert := notificationCA(t)
	smtpAddr, smtpMessages := smtpFixture(t, &cert, false)

	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Channel matrix"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	key := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "matrix key", "scopes": []string{"inference"}, "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyID := key["id"].(string)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.api_keys SET expires_at=now()+interval '1 day' WHERE id=$1", keyID); err != nil {
		t.Fatal(err)
	}
	var expiresAt time.Time
	if err := h.Pool.QueryRow(t.Context(), "SELECT expires_at FROM olp.api_keys WHERE id=$1", keyID).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}

	hookSecret := func(server *httptest.Server) map[string]any {
		return map[string]any{"webhook_url": server.URL + "/hook/tokenized?key=x"}
	}
	channelTypes := []string{"webhook", "slack", "msteams", "discord", "pagerduty", "email"}
	destinationBody := func(typ string, scoped bool) map[string]any {
		var body map[string]any
		switch typ {
		case "webhook":
			body = map[string]any{"url": webhookServer.URL, "secret": "sig-secret"}
		case "slack", "msteams":
			server := slackServer
			if typ == "msteams" {
				server = teamsServer
			}
			body = map[string]any{"type": typ, "url": server.URL, "secret": hookSecret(server)}
		case "discord":
			body = map[string]any{"type": "discord", "url": discordServer.URL, "secret": hookSecret(discordServer)}
		case "pagerduty":
			body = map[string]any{"type": "pagerduty", "url": pdServer.URL + "/enqueue", "secret": map[string]any{"routing_key": "rk-secret"}}
		case "email":
			body = map[string]any{"type": "email", "url": "smtps://" + smtpAddr,
				"configuration": map[string]any{"from": "alerts@olp.test", "to": []string{"ops@example.test"}, "ca_certificate": caPEM}}
		}
		body["name"] = typ + map[bool]string{false: "-i", true: "-p"}[scoped]
		if scoped {
			body["project_id"] = projectID
		}
		return body
	}
	installationDest, projectDest := map[string]string{}, map[string]string{}
	for _, typ := range channelTypes {
		installationDest[typ] = h.want(owner, "POST", "/api/v1/notifications/destinations", destinationBody(typ, false),
			map[string]string{"Idempotency-Key": uuid.NewString()}, 201)["id"].(string)
		projectDest[typ] = h.want(owner, "POST", "/api/v1/notifications/destinations", destinationBody(typ, true),
			map[string]string{"Idempotency-Key": uuid.NewString()}, 201)["id"].(string)
	}

	type ruleSpec struct {
		project bool
		extra   map[string]any
	}
	specs := map[string]ruleSpec{
		"budget.threshold":            {true, map[string]any{"subject_kind": "api_key", "subject_id": keyID, "window_kind": "day", "threshold_percent": 50}},
		"key.expiring":                {true, map[string]any{"subject_kind": "api_key", "subject_id": keyID}},
		"provider.grant.lapsed":       {false, nil},
		"budget.exhausted":            {true, map[string]any{"configuration": map[string]any{"cooldown_seconds": 120}}},
		"provider.circuit.open":       {false, map[string]any{"configuration": map[string]any{"cooldown_seconds": 120}}},
		"provider.circuit.closed":     {false, map[string]any{"configuration": map[string]any{"cooldown_seconds": 60}}},
		"provider.error_rate":         {false, map[string]any{"configuration": map[string]any{"threshold": "0.5", "window_seconds": 300}}},
		"route.latency":               {true, map[string]any{"configuration": map[string]any{"threshold": "800", "metric": "ttft"}}},
		"provider.credential.failing": {false, map[string]any{"configuration": map[string]any{"threshold": "2"}}},
		"model.retirement":            {true, map[string]any{"configuration": map[string]any{"lead_days": 10}}},
		"runtime.install_failed":      {false, map[string]any{"configuration": map[string]any{"cooldown_seconds": 60}}},
		"worker.stale":                {false, map[string]any{"configuration": map[string]any{"cooldown_seconds": 60}}},
		"report.spend":                {true, map[string]any{"configuration": map[string]any{"period": "weekly"}}},
	}
	if len(specs) != 13 {
		t.Fatalf("event matrix drifted: %d events", len(specs))
	}

	rules := map[string]map[string]string{} // channel -> event -> rule id
	for _, typ := range channelTypes {
		rules[typ] = map[string]string{}
		for event, spec := range specs {
			body := map[string]any{"name": typ + "-" + event, "event": event}
			if spec.project {
				body["project_id"] = projectID
				body["destination_id"] = projectDest[typ]
			} else {
				body["destination_id"] = installationDest[typ]
			}
			for k, v := range spec.extra {
				body[k] = v
			}
			created := h.want(owner, "POST", "/api/v1/notifications/rules", body,
				map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			rules[typ][event] = created["id"].(string)
		}
	}

	insertSignal := func(ruleID, event, dedup string, resolved bool) {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(),
			`INSERT INTO olp.notification_signal_states(rule_id,subject,active,incident) VALUES($1,$2,$3,1)
			 ON CONFLICT(rule_id,subject) DO UPDATE SET active=EXCLUDED.active,incident=1`,
			ruleID, "matrix:"+event, !resolved); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{
			"version": 1, "subject": "matrix:" + event, "incident": 1,
			"incident_key": "matrix:" + event,
			"evidence":     map[string]any{"note": "controlled fixture"},
		})
		if _, err := h.Pool.Exec(t.Context(),
			`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
			 VALUES($1,$2,$3,$4,$5,$6,'pending')`, uuid.NewString(), ruleID, event, dedup, resolved, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, typ := range channelTypes {
		for event, ruleID := range rules[typ] {
			dedup := "matrix:" + typ + ":" + event
			switch event {
			case "budget.threshold":
				if _, err := h.Pool.Exec(t.Context(),
					`INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status)
					 VALUES($1,$2,7,50,'8','10','pending')`, uuid.NewString(), ruleID); err != nil {
					t.Fatal(err)
				}
			case "key.expiring":
				if _, err := h.Pool.Exec(t.Context(),
					`INSERT INTO olp.notification_deliveries(id,rule_id,api_key_id,due_at,reason,payload,status)
					 VALUES($1,$2,$3,$4,'expiry',$5,'pending')`, uuid.NewString(), ruleID, keyID, expiresAt,
					[]byte(`{"api_key_id":"`+keyID+`","api_key_name":"matrix key","project_id":"`+projectID+`"}`)); err != nil {
					t.Fatal(err)
				}
			default:
				switch event {
				case "budget.exhausted", "provider.error_rate", "route.latency":
				default:
					insertSignal(ruleID, event, dedup, event == "provider.circuit.closed")
				}
			}
		}
	}

	release := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.runtime_releases(id,sequence,sha256,snapshot,created_by) VALUES($1,(SELECT COALESCE(max(sequence),0)+1 FROM olp.runtime_releases),$2,'{}'::json,(SELECT id FROM olp.users LIMIT 1))`,
		release, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	providerID := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'matrix-errors','openai','active','{}'::jsonb,$2,$3,(SELECT id FROM olp.users LIMIT 1))`,
		providerID, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag) VALUES(gen_random_uuid(),'matrix-latency',(SELECT id FROM olp.users LIMIT 1),1,gen_random_uuid(),gen_random_uuid())`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,started_at,completed_at,status_code,total_latency_ms)
		 SELECT gen_random_uuid(),$1,$2,'matrix-latency','chat','openai',now(),now(),500,2000 FROM generate_series(1,20)`,
		release, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.attempts(id,request_id,request_started_at,ordinal,provider_id,upstream_model,started_at,completed_at,status_code,routing,committed)
		 SELECT gen_random_uuid(),r.id,r.started_at,1,$1,'matrix-model',r.started_at,r.completed_at,500,'{"first_output_ms":2000}'::jsonb,true
		 FROM olp.requests r WHERE r.route_slug='matrix-latency'`, providerID); err != nil {
		t.Fatal(err)
	}
	exhaustedKey := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id)
		 VALUES($1,$2,$3,'matrix-exhausted',(SELECT id FROM olp.users LIMIT 1),'{"daily_cost_limit":"0.01"}'::jsonb,$4,$5)`,
		exhaustedKey, "matrix-exhausted", []byte{1}, uuid.NewString(), projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.api_key_cost_windows(api_key_id,window_kind,window_id,accrued,unpriced_attempts)
		 VALUES($1,'day',(SELECT window_id FROM olp.budget_window('day',now())),'5',0)`, exhaustedKey); err != nil {
		t.Fatal(err)
	}

	var smtpMu sync.Mutex
	smtpSeen := []string{}
	smtpStop := make(chan struct{})
	go func() {
		for {
			select {
			case raw, ok := <-smtpMessages:
				if !ok {
					return
				}
				smtpMu.Lock()
				smtpSeen = append(smtpSeen, raw)
				smtpMu.Unlock()
			case <-smtpStop:
				return
			}
		}
	}()
	defer close(smtpStop)

	// let the real worker drain until every seeded delivery is delivered
	deadline := time.Now().Add(90 * time.Second)
	for {
		var pending int64
		if err := h.Pool.QueryRow(t.Context(),
			`SELECT count(*) FROM olp.notification_deliveries WHERE status='pending'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d deliveries stuck pending", pending)
		}
		deliveryPass(t, h, alertPolicy())
	}

	for _, typ := range channelTypes[:5] {
		bodies, _ := posts[typ].snapshot()
		if len(bodies) != 13 {
			t.Fatalf("%s received %d posts, want 13: %v", typ, len(bodies), bodies)
		}
		for event := range specs {
			hits := 0
			for _, raw := range bodies {
				if strings.Contains(raw, event) {
					hits++
				}
			}
			if hits != 1 {
				t.Fatalf("%s delivered event %s %d times, want 1", typ, event, hits)
			}
		}
	}
	pdBodies, _ := posts["pagerduty"].snapshot()
	pdActions := map[string]string{}
	for _, raw := range pdBodies {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("pagerduty delivered body %s", raw)
		}
		action, _ := decoded["event_action"].(string)
		for event := range specs {
			if strings.Contains(raw, event) {
				pdActions[event] = action
			}
		}
	}
	for event, action := range pdActions {
		want := "trigger"
		if event == "provider.circuit.closed" {
			want = "resolve"
		}
		if action != want {
			t.Fatalf("pagerduty %s action=%s, want %s", event, action, want)
		}
	}
	if len(pdActions) != 13 {
		t.Fatalf("pagerduty carried %d events, want 13", len(pdActions))
	}
	smtpDeadline := time.Now().Add(5 * time.Second)
	for {
		smtpMu.Lock()
		count := len(smtpSeen)
		smtpMu.Unlock()
		if count >= 13 || time.Now().After(smtpDeadline) {
			break
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	smtpMu.Lock()
	defer smtpMu.Unlock()
	if len(smtpSeen) != 13 {
		t.Fatalf("smtp received %d of 13 messages", len(smtpSeen))
	}
	for event := range specs {
		hits := 0
		for _, raw := range smtpSeen {
			if strings.Contains(raw, event) {
				hits++
			}
		}
		if hits != 1 {
			t.Fatalf("smtp carried event %s %d times, want 1", event, hits)
		}
	}

	var delivered, failed int64
	if err := h.Pool.QueryRow(t.Context(),
		`SELECT count(*) FILTER(WHERE status='delivered'),count(*) FILTER(WHERE status='failed') FROM olp.notification_deliveries`).Scan(&delivered, &failed); err != nil {
		t.Fatal(err)
	}
	if delivered != 78 || failed != 0 {
		t.Fatalf("delivered=%d failed=%d, want 78/0", delivered, failed)
	}
}
