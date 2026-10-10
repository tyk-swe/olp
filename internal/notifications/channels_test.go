package notifications

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/egress"
)

func testPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

func postCollector(t *testing.T) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var got [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, buf)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	return server, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}
}

func testBody() []byte {
	return []byte(`{"event":"budget.threshold","rule_id":"r1","rule_name":"Spend","subject_kind":"api_key","window_kind":"day","window_id":"42","threshold_percent":80,"accrued":"8.0","limit":"10","currency":"usd","delivery_id":"d1"}`)
}

func TestSendWebhookPreservedSemantics(t *testing.T) {
	server, _ := postCollector(t)
	var signature string
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signature = r.Header.Get("X-OLP-Signature")
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		w.Header().Set("X-Bod", string(buf[:n]))
		w.WriteHeader(204)
	})
	d := Destination{URL: server.URL}
	if err := ValidateDestination(testPolicy(), d, []byte("sig")); err != nil {
		t.Fatal(err)
	}
	if err := Send(t.Context(), testPolicy(), d, []byte("sig"), testBody()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(signature, "sha256=") || signature == "sha256=" {
		t.Fatalf("signature %q", signature)
	}
}

func TestChannelOriginRules(t *testing.T) {
	policy := testPolicy()
	for _, url := range []string{
		"https://hooks.example.com/hooks/tok", "https://hooks.example.com?x=1",
		"https://user@hooks.example.com", "https://hooks.example.com#f",
	} {
		d := Destination{Type: "slack", URL: url}
		if err := ValidateDestination(policy, d, nil); err == nil {
			t.Fatalf("%s accepted as an origin", url)
		}
	}
	server, _ := postCollector(t)
	d := Destination{Type: "slack", URL: server.URL}
	if err := ValidateDestination(policy, d, nil); err == nil {
		t.Fatal("slack destination accepted without its webhook_url secret")
	}
	if err := ValidateDestination(policy, d, []byte(`{"webhook_url":"https://other.example.com/hook"}`)); err == nil {
		t.Fatal("cross-origin webhook_url accepted")
	}
	if err := ValidateDestination(policy, d, []byte(`{"webhook_url":"`+server.URL+`/tok","extra":1}`)); err == nil {
		t.Fatal("unknown secret key accepted")
	}
	if err := ValidateDestination(policy, d, []byte(`{"webhook_url":"`+server.URL+`/tok"}`)); err != nil {
		t.Fatalf("valid slack secret refused: %v", err)
	}
}

func TestSendChatChannelsShape(t *testing.T) {
	server, got := postCollector(t)
	secret := []byte(`{"webhook_url":"` + server.URL + `/tokenized-path?key=abc"}`)
	for _, typ := range []string{"slack", "msteams", "discord"} {
		d := Destination{Type: typ, URL: server.URL}
		if err := ValidateDestination(testPolicy(), d, secret); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if err := Send(t.Context(), testPolicy(), d, secret, testBody()); err != nil {
			t.Fatalf("%s send: %v", typ, err)
		}
	}
	bodies := got()
	if len(bodies) != 3 {
		t.Fatalf("posts = %d", len(bodies))
	}
	var slack map[string]any
	if err := json.Unmarshal(bodies[0], &slack); err != nil {
		t.Fatal(err)
	}
	blocks := slack["blocks"].([]any)
	if len(blocks) < 2 || blocks[0].(map[string]any)["type"] != "header" || slack["text"] == "" {
		t.Fatalf("slack payload %v", slack)
	}
	var teams map[string]any
	if err := json.Unmarshal(bodies[1], &teams); err != nil {
		t.Fatal(err)
	}
	attachment := teams["attachments"].([]any)[0].(map[string]any)
	if attachment["contentType"] != "application/vnd.microsoft.card.adaptive" ||
		attachment["content"].(map[string]any)["version"] != "1.4" {
		t.Fatalf("teams payload %v", teams)
	}
	var discord map[string]any
	if err := json.Unmarshal(bodies[2], &discord); err != nil {
		t.Fatal(err)
	}
	if _, ok := discord["content"]; !ok || len(discord["embeds"].([]any)) != 1 {
		t.Fatalf("discord payload %v", discord)
	}
}

func TestSendPagerDutyTriggerAndResolve(t *testing.T) {
	var lastDedup string
	var mu sync.Mutex
	var received [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, buf)
		mu.Unlock()
		var decoded map[string]any
		_ = json.Unmarshal(buf, &decoded)
		lastDedup, _ = decoded["dedup_key"].(string)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success","dedup_key":"` + lastDedup + `"}`))
	}))
	t.Cleanup(server.Close)
	got := func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), received...)
	}
	secret := []byte(`{"routing_key":"rk-test"}`)
	d := Destination{Type: "pagerduty", URL: server.URL + "/enqueue"}
	if err := ValidateDestination(testPolicy(), d, secret); err != nil {
		t.Fatal(err)
	}
	trigger := []byte(`{"event":"budget.exhausted","rule_id":"r2","rule_name":"Exhaust","resolved":false,"incident_key":"inc-9","evidence":{"accrued":"12"}}`)
	if err := Send(t.Context(), testPolicy(), d, secret, trigger); err != nil {
		t.Fatal(err)
	}
	resolve := []byte(`{"event":"budget.exhausted","rule_id":"r2","rule_name":"Exhaust","resolved":true,"incident_key":"inc-9"}`)
	if err := Send(t.Context(), testPolicy(), d, secret, resolve); err != nil {
		t.Fatal(err)
	}
	posted := got()
	if len(posted) != 2 {
		t.Fatalf("posts = %d", len(posted))
	}
	var out, back map[string]any
	if err := json.Unmarshal(posted[0], &out); err != nil {
		t.Fatalf("unmarshal trigger %s: %v", posted[0], err)
	}
	if err := json.Unmarshal(posted[1], &back); err != nil {
		t.Fatalf("unmarshal resolve %s: %v", posted[1], err)
	}
	for _, want := range []struct {
		m      map[string]any
		action string
	}{{out, "trigger"}, {back, "resolve"}} {
		if want.m["routing_key"] != "rk-test" || want.m["event_action"] != want.action || want.m["dedup_key"] != "inc-9" {
			t.Fatalf("pagerduty payload %v", want.m)
		}
		payload := want.m["payload"].(map[string]any)
		if payload["source"] != "OpenLLMProxy" || payload["severity"] != "warning" || payload["summary"] == "" {
			t.Fatalf("pagerduty envelope %v", payload)
		}
	}
	if _, ok := out["payload"].(map[string]any)["custom_details"]; !ok {
		t.Fatal("custom_details missing")
	}
	if strings.Contains(string(posted[0]), "rk-test") && !strings.Contains(string(posted[0]), "routing_key") {
		t.Fatal("routing key leaked outside routing_key")
	}
}

func TestSendPagerDutyDedupFallback(t *testing.T) {
	body := []byte(`{"event":"budget.threshold","rule_id":"r1","rule_name":"Spend","window_id":"42","delivery_id":"d1"}`)
	payload, err := pagerDutyPayload("rk", body)
	if err != nil {
		t.Fatal(err)
	}
	if payload["dedup_key"] != "r1:budget.threshold:42" {
		t.Fatalf("fallback dedup %v", payload)
	}
}

func TestSendHTTPFailures(t *testing.T) {
	for status, code := range map[int]string{301: "network", 403: "http_4xx", 500: "http_5xx"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == 301 {
				w.Header().Set("Location", "http://127.0.0.1:1/")
			}
			w.WriteHeader(status)
		}))
		d := Destination{Type: "pagerduty", URL: server.URL}
		secret := []byte(`{"routing_key":"rk"}`)
		err := Send(t.Context(), testPolicy(), d, secret, testBody())
		server.Close()
		if err == nil || SendErrorCode(err) != code {
			t.Fatalf("status %d err=%v code=%q want %q", status, err, SendErrorCode(err), code)
		}
	}
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	d := Destination{Type: "pagerduty", URL: "http://127.0.0.1:8080"}
	secret := []byte(`{"routing_key":"rk"}`)
	if err := ValidateDestination(policy, d, secret); err == nil {
		t.Fatal("egress-denied destination accepted")
	}
}

func caAndServerCert(t *testing.T, dnsOrIP string) (caPEM string, serverTLS tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
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
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "smtp.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{dnsOrIP},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err = tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustEC(t, srvKey)}))
	if err != nil {
		t.Fatal(err)
	}
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return caPEM, serverTLS
}

func mustEC(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

type smtpFake struct {
	listener net.Listener
	tls      bool
	starttls bool
	messages chan []string
	envelope chan string
}

func startSMTPFake(t *testing.T, cert *tls.Certificate, mode string) *smtpFake {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &smtpFake{listener: listener, messages: make(chan []string, 4), envelope: make(chan string, 64), tls: mode == "smtps", starttls: mode == "starttls"}
	go f.serve(cert)
	t.Cleanup(func() { listener.Close() })
	return f
}

func (f *smtpFake) serve(cert *tls.Certificate) {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn, cert)
	}
}

func (f *smtpFake) handle(conn net.Conn, cert *tls.Certificate) {
	defer conn.Close()
	if f.tls {
		conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	}
	reader := bufio.NewReader(conn)
	write := func(s string) { _, _ = conn.Write([]byte(s)) }
	write("220 fake.test ESMTP\r\n")
	var lines []string
	inData := false
	for {
		line, err := reader.ReadString(0x0a)
		if err != nil {
			return
		}
		if inData {
			lines = append(lines, line)
			if strings.TrimSpace(line) == "." {
				inData = false
				write("250 queued\r\n")
				select {
				case f.messages <- lines:
				default:
				}
			}
			continue
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if f.starttls {
				write("250-fake.test\r\n250-STARTTLS\r\n250-AUTH PLAIN LOGIN\r\n250 OK\r\n")
			} else {
				write("250 fake.test\r\n")
			}
		case command == "STARTTLS":
			if !f.starttls {
				write("454 not offered\r\n")
				continue
			}
			write("220 Go ahead\r\n")
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
			reader = bufio.NewReader(conn)
		case strings.HasPrefix(command, "AUTH"):
			write("235 authenticated\r\n")
		case strings.HasPrefix(command, "MAIL"), strings.HasPrefix(command, "RCPT"):
			select {
			case f.envelope <- line:
			default:
			}
			write("250 ok\r\n")
		case command == "DATA":
			write("354 end with .\r\n")
			inData = true
		case command == "QUIT":
			write("221 bye\r\n")
			return
		case command == "RSET":
			write("250 ok\r\n")
		default:
			write("502 unsupported\r\n")
		}
	}
}

func emailDestination(url, caPEM string) Destination {
	config, _ := json.Marshal(map[string]any{
		"from":           "alerts@olp.test",
		"to":             []string{"ops@example.test", "dev@example.test"},
		"subject_prefix": "[OLP]",
		"ca_certificate": caPEM,
	})
	return Destination{Type: "email", URL: url, Configuration: config}
}

func TestSendEmailImplicitTLS(t *testing.T) {
	caPEM, cert := caAndServerCert(t, "127.0.0.1")
	fake := startSMTPFake(t, &cert, "smtps")
	d := emailDestination("smtps://"+fake.listener.Addr().String(), caPEM)
	if err := ValidateDestination(testPolicy(), d, nil); err != nil {
		t.Fatal(err)
	}
	if err := Send(t.Context(), testPolicy(), d, nil, testBody()); err != nil {
		t.Fatalf("smtps send: %v", err)
	}
	select {
	case message := <-fake.messages:
		full := strings.Join(message, "")
		if !strings.Contains(full, "Subject:") || !strings.Contains(full, "Message-ID:") ||
			!strings.Contains(full, "alerts@olp.test") {
			t.Fatalf("message headers %q", full)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
	}
}

func TestSendEmailStartTLS(t *testing.T) {
	caPEM, cert := caAndServerCert(t, "127.0.0.1")
	fake := startSMTPFake(t, &cert, "starttls")
	d := emailDestination("smtp+starttls://"+fake.listener.Addr().String(), caPEM)
	secret := []byte(`{"username":"u","password":"p"}`)
	if err := ValidateDestination(testPolicy(), d, secret); err != nil {
		t.Fatal(err)
	}
	if err := Send(t.Context(), testPolicy(), d, secret, testBody()); err != nil {
		t.Fatalf("starttls send: %v", err)
	}
	select {
	case <-fake.messages:
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
	}
}

func TestSendEmailRefusesPlainAndBadCert(t *testing.T) {
	caPEM, cert := caAndServerCert(t, "127.0.0.1")
	fake := startSMTPFake(t, &cert, "plain")
	d := emailDestination("smtp+starttls://"+fake.listener.Addr().String(), caPEM)
	err := Send(t.Context(), testPolicy(), d, nil, testBody())
	if err == nil || SendErrorCode(err) != "invalid_destination" {
		t.Fatalf("no-STARTTLS accepted: %v", err)
	}
	otherCA, _ := caAndServerCert(t, "127.0.0.1")
	d2 := emailDestination("smtps://"+fake.listener.Addr().String(), otherCA)
	fake2 := startSMTPFake(t, &cert, "smtps")
	d2.URL = "smtps://" + fake2.listener.Addr().String()
	if err := Send(t.Context(), testPolicy(), d2, nil, testBody()); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestSendEmailEgressDenied(t *testing.T) {
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	caPEM, _ := caAndServerCert(t, "127.0.0.1")
	d := emailDestination("smtps://127.0.0.1:4465", caPEM)
	if err := ValidateDestination(policy, d, nil); err == nil {
		t.Fatal("egress-denied smtp accepted")
	}
	if err := Send(t.Context(), policy, d, nil, testBody()); err == nil {
		t.Fatal("egress-denied smtp sent")
	}
}

func TestEmailSecretAndConfigValidation(t *testing.T) {
	caPEM, _ := caAndServerCert(t, "127.0.0.1")
	d := emailDestination("smtps://127.0.0.1:4465", caPEM)
	for _, secret := range [][]byte{
		[]byte(`{"username":"u"}`),
		[]byte(`{"username":"u","password":"p","extra":1}`),
		[]byte(`{"username":"u\r\n","password":"p"}`),
	} {
		if err := ValidateDestination(testPolicy(), d, secret); err == nil {
			t.Fatalf("secret %s accepted", secret)
		}
	}
	if err := ValidateDestination(testPolicy(), emailDestination("smtps://127.0.0.1:4465", ""), nil); err != nil {
		t.Fatalf("system-trust email config refused: %v", err)
	}
	for _, bad := range []Destination{
		{Type: "email", URL: "smtp://127.0.0.1:25"},
		{Type: "email", URL: "smtps://127.0.0.1"},
		{Type: "email", URL: "smtps://127.0.0.1:465/path"},
		{Type: "email", URL: "smtps://user@127.0.0.1:465"},
	} {
		bad.Configuration = d.Configuration
		if err := ValidateDestination(testPolicy(), bad, nil); err == nil {
			t.Fatalf("url %s accepted", bad.URL)
		}
	}
}

func TestSendEveryEventToAllChannels(t *testing.T) {
	policy := testPolicy()
	posts := map[string][][]byte{}
	var mu sync.Mutex
	receiver := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			mu.Lock()
			posts[name] = append(posts[name], buf)
			mu.Unlock()
			w.WriteHeader(204)
		}))
	}
	servers := map[string]*httptest.Server{}
	for _, name := range []string{"webhook", "slack", "msteams", "discord"} {
		servers[name] = receiver(name)
		t.Cleanup(servers[name].Close)
	}
	pdReceived := [][]byte{}
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		pdReceived = append(pdReceived, buf)
		mu.Unlock()
		var decoded map[string]any
		_ = json.Unmarshal(buf, &decoded)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success","dedup_key":"` + decoded["dedup_key"].(string) + `"}`))
	}))
	t.Cleanup(pd.Close)

	caPEM, cert := caAndServerCert(t, "127.0.0.1")
	fake := startSMTPFake(t, &cert, "smtps")
	smtpAddr := fake.listener.Addr().String()
	smtpMessages := fake.messages

	hookSecret := func(u *url.URL) []byte {
		return []byte(`{"webhook_url":"` + u.String() + `/hook?k=1"}`)
	}
	config, _ := json.Marshal(map[string]any{"from": "alerts@olp.test", "to": []string{"ops@example.test"}, "ca_certificate": caPEM})
	channels := map[string]struct {
		destination Destination
		secret      []byte
	}{
		"webhook":   {Destination{Type: "webhook", URL: servers["webhook"].URL}, []byte("sig")},
		"slack":     {Destination{Type: "slack", URL: servers["slack"].URL}, hookSecret(mustParse(t, servers["slack"].URL))},
		"msteams":   {Destination{Type: "msteams", URL: servers["msteams"].URL}, hookSecret(mustParse(t, servers["msteams"].URL))},
		"discord":   {Destination{Type: "discord", URL: servers["discord"].URL}, hookSecret(mustParse(t, servers["discord"].URL))},
		"pagerduty": {Destination{Type: "pagerduty", URL: pd.URL}, []byte(`{"routing_key":"rk"}`)},
		"email":     {Destination{Type: "email", URL: "smtps://" + smtpAddr, Configuration: config}, nil},
	}
	for name, channel := range channels {
		if err := ValidateDestination(policy, channel.destination, channel.secret); err != nil {
			t.Fatalf("%s validate: %v", name, err)
		}
		for _, event := range Events {
			body := []byte(`{"event":"` + event + `","rule_id":"r1","rule_name":"Alert","subject":"notification_delivery","incident":1,"incident_key":"inc:` + name + `","delivery_id":"d1"}`)
			if err := Send(t.Context(), policy, channel.destination, channel.secret, body); err != nil {
				t.Fatalf("%s %s: %v", name, event, err)
			}
			if name == "email" {
				select {
				case lines := <-smtpMessages:
					message := strings.Join(lines, "")
					if !strings.Contains(message, event) {
						t.Fatalf("email for %s missing event: %q", event, message)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("email for %s not received", event)
				}
			}
		}
	}
	for _, name := range []string{"webhook", "slack", "msteams", "discord"} {
		mu.Lock()
		got := append([][]byte(nil), posts[name]...)
		mu.Unlock()
		if len(got) != len(Events) {
			t.Fatalf("%s posts = %d, want %d", name, len(got), len(Events))
		}
		for i, event := range Events {
			if !strings.Contains(string(got[i]), event) {
				t.Fatalf("%s post %d missing %s", name, i, event)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pdReceived) != len(Events) {
		t.Fatalf("pagerduty posts = %d, want %d", len(pdReceived), len(Events))
	}
	for i, event := range Events {
		var decoded map[string]any
		if err := json.Unmarshal(pdReceived[i], &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["dedup_key"] != "inc:pagerduty" {
			t.Fatalf("pagerduty %s dedup %v", event, decoded["dedup_key"])
		}
		details := decoded["payload"].(map[string]any)["custom_details"].(map[string]any)
		if details["event"] != event {
			t.Fatalf("pagerduty %s event %v", event, details["event"])
		}
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSendRefusesWithdrawnHTTPExemption(t *testing.T) {
	server, _ := postCollector(t)
	d := Destination{Type: "slack", URL: server.URL}
	secret := []byte(`{"webhook_url":"` + server.URL + `/hook"}`)
	if err := ValidateDestination(testPolicy(), d, secret); err != nil {
		t.Fatal(err)
	}
	stricter := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	if err := Send(t.Context(), stricter, d, secret, testBody()); err == nil || SendErrorCode(err) != "invalid_destination" {
		t.Fatalf("send after HTTP exemption withdrawn: %v", err)
	}
	bad := []byte(`{"webhook_url":"` + server.URL + `/hook","other":"x"}`)
	if err := Send(t.Context(), testPolicy(), d, bad, testBody()); err == nil {
		t.Fatal("unknown secret key sent")
	}
}

func TestPagerDutyRejectsBadAck(t *testing.T) {
	for name, respond := range map[string]func(w http.ResponseWriter){
		"malformed": func(w http.ResponseWriter) { w.WriteHeader(202); _, _ = w.Write([]byte("nope")) },
		"false": func(w http.ResponseWriter) {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"status":"error","dedup_key":"x"}`))
		},
		"mismatched": func(w http.ResponseWriter) {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"status":"success","dedup_key":"other"}`))
		},
		"status204": func(w http.ResponseWriter) { w.WriteHeader(204) },
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			respond(w)
		}))
		d := Destination{Type: "pagerduty", URL: server.URL}
		err := Send(t.Context(), testPolicy(), d, []byte(`{"routing_key":"rk"}`), testBody())
		server.Close()
		if err == nil {
			t.Fatalf("%s ack accepted", name)
		}
	}
}

func TestSendEmailCancellationDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		wg.Wait()
	})
	caPEM, _ := caAndServerCert(t, "127.0.0.1")
	d := emailDestination("smtps://"+listener.Addr().String(), caPEM)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Send(ctx, testPolicy(), d, nil, testBody()) }()
	var remote net.Conn
	select {
	case remote = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("connection never arrived")
	}
	t.Cleanup(func() { remote.Close() })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled send returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled send hung")
	}
	if err := remote.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// A cancelled handshake must have closed the raw connection, so draining
	// it (including any buffered TLS bytes) must end in EOF, never a timeout.
	if _, err := io.Copy(io.Discard, remote); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("connection stayed open after cancel: %v", err)
		}
		t.Fatalf("remote drain = %v, want clean EOF", err)
	}
}

func TestSendEmailNamedAddressesEnvelope(t *testing.T) {
	caPEM, cert := caAndServerCert(t, "127.0.0.1")
	fake := startSMTPFake(t, &cert, "smtps")
	config, _ := json.Marshal(map[string]any{
		"from": "Alerts <alerts@olp.test>", "to": []string{"Ops <ops@example.test>", "dev@example.test"},
		"subject_prefix": "[OLP]", "ca_certificate": caPEM,
	})
	d := Destination{Type: "email", URL: "smtps://" + fake.listener.Addr().String(), Configuration: config}
	if err := ValidateDestination(testPolicy(), d, nil); err != nil {
		t.Fatal(err)
	}
	if err := Send(t.Context(), testPolicy(), d, nil, testBody()); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for i := 0; i < 3; i++ {
		select {
		case line := <-fake.envelope:
			commands = append(commands, strings.TrimSpace(line))
		case <-time.After(5 * time.Second):
			t.Fatal("envelope commands incomplete")
		}
	}
	want := []string{
		"MAIL FROM:<alerts@olp.test>",
		"RCPT TO:<ops@example.test>",
		"RCPT TO:<dev@example.test>",
	}
	for i, w := range want {
		if commands[i] != w {
			t.Fatalf("envelope[%d] = %q, want %q", i, commands[i], w)
		}
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	long := strings.Repeat("x", 300)
	if got := truncate(long, 200); utf8.RuneCountInString(got) != 200 {
		t.Fatalf("ASCII truncate = %d runes", utf8.RuneCountInString(got))
	}
	mixed := strings.Repeat("界", 300)
	if got := truncate(mixed, 200); utf8.RuneCountInString(got) != 200 || !utf8.ValidString(got) {
		t.Fatalf("unicode truncate = %d runes valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if truncate("short", 200) != "short" {
		t.Fatal("short string truncated")
	}
	payload := channelPayload(Destination{Type: "slack"}, json.RawMessage(`{"event":"x","rule_name":"`+strings.Repeat("界", 500)+`"}`))
	blocks := payload.(map[string]any)["blocks"].([]map[string]any)
	title := blocks[0]["text"].(map[string]any)["text"].(string)
	if utf8.RuneCountInString(title) > 150 || !utf8.ValidString(title) {
		t.Fatalf("slack title %d runes", utf8.RuneCountInString(title))
	}
	discordPayload := channelPayload(Destination{Type: "discord"}, json.RawMessage(`{"event":"x","rule_name":"`+strings.Repeat("é", 10000)+`"}`))
	m := discordPayload.(map[string]any)
	if utf8.RuneCountInString(m["content"].(string)) > 2000 {
		t.Fatal("discord content exceeds 2000 runes")
	}
	embeds := m["embeds"].([]map[string]any)
	if utf8.RuneCountInString(embeds[0]["description"].(string)) > 4096 {
		t.Fatal("discord description exceeds 4096 runes")
	}
	if utf8.RuneCountInString(embeds[0]["title"].(string)) > 256 {
		t.Fatal("discord title exceeds 256 runes")
	}
	if parse := m["allowed_mentions"].(map[string]any)["parse"].([]string); len(parse) != 0 {
		t.Fatalf("allowed_mentions %v", parse)
	}
}

func TestSMTPSecretStrictness(t *testing.T) {
	for _, tc := range []struct {
		raw      []byte
		wantUser string
		wantErr  bool
	}{
		{nil, "", false},
		{[]byte(`{}`), "", false},
		{[]byte(` { } `), "", false},
		{[]byte("{\n}\n"), "", false},
		{[]byte(`{"username":"u","password":"p"}`), "u", false},
		{[]byte(`{"username":"u"}`), "", true},
		{[]byte(`{"username":"u","password":"p","other":"x"}`), "", true},
		{[]byte(`{"a":"x","b":"y"}`), "", true},
		{[]byte(`{"username":null,"password":null}`), "", true},
		{[]byte(`{"username":"","password":""}`), "", true},
		{[]byte(`{"username":"u` + "\x00" + `","password":"p"}`), "", true},
		{[]byte(`{"username":"u\n","password":"p"}`), "", true},
	} {
		user, pass, err := smtpSecret(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s accepted", tc.raw)
			}
			continue
		}
		if err != nil || user != tc.wantUser || (user == "") != (pass == "") {
			t.Fatalf("%s = %q/%q %v", tc.raw, user, pass, err)
		}
	}
}

func TestSMTPIPv6Hostname(t *testing.T) {
	_, host, err := smtpTarget("smtps://[::1]:465")
	if err != nil {
		t.Fatal(err)
	}
	hostname, _, err := net.SplitHostPort(host)
	if err != nil || hostname != "::1" {
		t.Fatalf("IPv6 hostname %q", hostname)
	}
	if _, _, err = smtpTarget("smtps://[::1]"); err == nil {
		t.Fatal("missing port accepted")
	}
}
