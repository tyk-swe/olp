package notifications

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
)

func testPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

func testBody() []byte {
	return []byte(`{"event":"budget.threshold","rule_id":"r1","rule_name":"Spend","subject_kind":"api_key","window_kind":"day","window_id":"42","threshold_percent":80,"accrued":"8.0","limit":"10","currency":"usd","delivery_id":"d1"}`)
}

func TestSendHTTPFailures(t *testing.T) {
	sender := NewSender(*testPolicy())
	for status, code := range map[int]string{301: redirectCode, 403: http4xxCode, 500: http5xxCode} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == 301 {
				w.Header().Set("Location", "http://127.0.0.1:1/")
			}
			w.WriteHeader(status)
		}))
		d := Destination{Type: "pagerduty", URL: server.URL}
		secret := []byte(`{"routing_key":"rk"}`)
		err := sender.Send(t.Context(), d, secret, testBody())
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
	deniedSender := NewSender(*policy)
	if SendErrorCode(deniedSender.Send(t.Context(), d, secret, testBody())) != invalidDestinationCode {
		t.Fatal("egress-denied destination was not classified as invalid")
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
	smtpFakeOptions
	listener net.Listener
	tls      bool
	starttls bool
	messages chan []string
	envelope chan string
}

type smtpFakeOptions struct {
	rcptReply   string
	closeOnQuit bool
}

func startSMTPFake(t *testing.T, cert *tls.Certificate, mode string, options smtpFakeOptions) *smtpFake {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &smtpFake{smtpFakeOptions: options, listener: listener, messages: make(chan []string, 4), envelope: make(chan string, 64), tls: mode == "smtps", starttls: mode == "starttls"}
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
		case strings.HasPrefix(command, "MAIL"):
			select {
			case f.envelope <- line:
			default:
			}
			write("250 ok\r\n")
		case strings.HasPrefix(command, "RCPT"):
			select {
			case f.envelope <- line:
			default:
			}
			reply := f.rcptReply
			if reply == "" {
				reply = "250 ok"
			}
			write(reply + "\r\n")
		case command == "DATA":
			write("354 end with .\r\n")
			inData = true
		case command == "QUIT":
			if !f.closeOnQuit {
				write("221 bye\r\n")
			}
			return
		case command == "RSET":
			write("250 ok\r\n")
		default:
			write("502 unsupported\r\n")
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
	smtpsFake := startSMTPFake(t, &cert, "smtps", smtpFakeOptions{})
	starttlsFake := startSMTPFake(t, &cert, "starttls", smtpFakeOptions{})

	hookSecret := func(u *url.URL) []byte {
		return []byte(`{"webhook_url":"` + u.String() + `/hook?k=1"}`)
	}
	config, _ := json.Marshal(map[string]any{"from": "alerts@olp.test", "to": []string{"ops@example.test"}, "ca_certificate": caPEM})
	channels := map[string]struct {
		destination Destination
		secret      []byte
	}{
		"webhook":        {Destination{Type: "webhook", URL: servers["webhook"].URL}, []byte("sig")},
		"slack":          {Destination{Type: "slack", URL: servers["slack"].URL}, hookSecret(mustParse(t, servers["slack"].URL))},
		"msteams":        {Destination{Type: "msteams", URL: servers["msteams"].URL}, hookSecret(mustParse(t, servers["msteams"].URL))},
		"discord":        {Destination{Type: "discord", URL: servers["discord"].URL}, hookSecret(mustParse(t, servers["discord"].URL))},
		"pagerduty":      {Destination{Type: "pagerduty", URL: pd.URL}, []byte(`{"routing_key":"rk"}`)},
		"email-smtps":    {Destination{Type: emailType, URL: "smtps://" + smtpsFake.listener.Addr().String(), Configuration: config}, nil},
		"email-starttls": {Destination{Type: emailType, URL: "smtp+starttls://" + starttlsFake.listener.Addr().String(), Configuration: config}, nil},
	}
	sender := NewSender(*policy)
	for name, channel := range channels {
		if err := ValidateDestination(policy, channel.destination, channel.secret); err != nil {
			t.Fatalf("%s validate: %v", name, err)
		}
		for _, event := range Events {
			body := []byte(`{"event":"` + event + `","rule_id":"r1","rule_name":"Alert","subject":"notification_delivery","incident":1,"incident_key":"inc:` + name + `","delivery_id":"d1"}`)
			if err := sender.Send(t.Context(), channel.destination, channel.secret, body); err != nil {
				t.Fatalf("%s %s: %v", name, event, err)
			}
			if strings.HasPrefix(name, "email-") {
				smtpMessages := smtpsFake.messages
				if name == "email-starttls" {
					smtpMessages = starttlsFake.messages
				}
				select {
				case lines := <-smtpMessages:
					message := strings.Join(lines, "")
					_, encodedBody, ok := strings.Cut(message, "\r\n\r\n")
					if !ok {
						t.Fatalf("email for %s has no body", event)
					}
					decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(encodedBody)))
					if err != nil || !strings.Contains(string(decoded), event) {
						t.Fatalf("email for %s missing decoded event: %q err=%v", event, decoded, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s email for %s not received", name, event)
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

func TestEmailSMTPReplies(t *testing.T) {
	caPEM, cert := caAndServerCert(t, "127.0.0.1")
	config, err := json.Marshal(map[string]any{
		"from":           "alerts@olp.test",
		"to":             []string{"ops@olp.test"},
		"ca_certificate": caPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		rcptReply   string
		closeOnQuit bool
		wantCode    string
	}{
		{name: "permanent recipient failure", rcptReply: "550 no such user", wantCode: smtp5xxCode},
		{name: "temporary recipient failure", rcptReply: "451 try later", wantCode: smtp4xxCode},
		{name: "close after acceptance", closeOnQuit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := startSMTPFake(t, &cert, "smtps", smtpFakeOptions{
				rcptReply: test.rcptReply, closeOnQuit: test.closeOnQuit,
			})
			sender := NewSender(*testPolicy())
			destination := Destination{
				Type: emailType, URL: "smtps://" + fake.listener.Addr().String(), Configuration: config,
			}
			err := sender.Send(t.Context(), destination, nil, testBody())
			if test.wantCode != "" {
				if got := SendErrorCode(err); got != test.wantCode {
					t.Fatalf("SendErrorCode() = %q, want %q (err %v)", got, test.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			select {
			case message := <-fake.messages:
				if len(message) == 0 {
					t.Fatal("fake received an empty message")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("fake did not receive the accepted message")
			}
		})
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

func TestValidateDestination(t *testing.T) {
	emailConfig, _ := json.Marshal(map[string]any{"from": "alerts@olp.test", "to": []string{"ops@olp.test"}})
	tests := []struct {
		name        string
		destination Destination
		secret      []byte
		nilPolicy   bool
		wantShapeOK bool
		wantFullOK  bool
	}{
		{
			name:        "webhook without secret",
			destination: Destination{URL: "http://127.0.0.1:1"},
			wantShapeOK: true,
			wantFullOK:  true,
		},
		{
			name:        "webhook with configuration",
			destination: Destination{URL: "http://127.0.0.1:1", Configuration: json.RawMessage(`{"x":1}`)},
		},
		{
			name:        "webhook secret too large",
			destination: Destination{URL: "http://127.0.0.1:1"},
			secret:      []byte(strings.Repeat("x", 1025)),
			wantShapeOK: true,
		},
		{
			name:        "chat origin has path",
			destination: Destination{Type: slackType, URL: "http://127.0.0.1:1/x"},
			secret:      []byte(`{"webhook_url":"http://127.0.0.1:1/hook"}`),
		},
		{
			name:        "chat without secret",
			destination: Destination{Type: slackType, URL: "http://127.0.0.1:1"},
			wantShapeOK: true,
		},
		{
			name:        "chat hook on another origin",
			destination: Destination{Type: slackType, URL: "http://127.0.0.1:1"},
			secret:      []byte(`{"webhook_url":"http://127.0.0.1:2/hook"}`),
			wantShapeOK: true,
		},
		{
			name:        "pagerduty empty routing key",
			destination: Destination{Type: pagerDutyType, URL: "http://127.0.0.1:1"},
			secret:      []byte(`{"routing_key":""}`),
			wantShapeOK: true,
		},
		{
			name:        "email without configuration",
			destination: Destination{Type: emailType, URL: "smtps://127.0.0.1:25"},
		},
		{
			name:        "email with partial credentials",
			destination: Destination{Type: emailType, URL: "smtps://127.0.0.1:25", Configuration: emailConfig},
			secret:      []byte(`{"username":"u"}`),
			wantShapeOK: true,
		},
		{
			name:        "unknown type",
			destination: Destination{Type: "sms", URL: "http://127.0.0.1:1"},
		},
		{
			name:        "nil policy",
			destination: Destination{URL: "http://127.0.0.1:1"},
			nilPolicy:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy()
			if test.nilPolicy {
				policy = nil
			}
			shapeErr := ValidateDestinationShape(policy, test.destination)
			fullErr := ValidateDestination(policy, test.destination, test.secret)
			if (shapeErr == nil) != test.wantShapeOK {
				t.Errorf("shape error = %v, want success %v", shapeErr, test.wantShapeOK)
			}
			if (fullErr == nil) != test.wantFullOK {
				t.Errorf("full error = %v, want success %v", fullErr, test.wantFullOK)
			}
		})
	}
}

func TestWebhookSignsAndRefusesRedirects(t *testing.T) {
	body := testBody()
	secret := []byte("signing-key")
	type webhookRequest struct {
		body        []byte
		signature   string
		contentType string
	}
	received := make(chan webhookRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ := io.ReadAll(r.Body)
		received <- webhookRequest{
			body:        receivedBody,
			signature:   r.Header.Get("X-OLP-Signature"),
			contentType: r.Header.Get("Content-Type"),
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender := NewSender(*testPolicy())
	destination := Destination{URL: server.URL}
	if err := sender.Send(t.Context(), destination, secret, body); err != nil {
		t.Fatal(err)
	}
	signed := <-received
	sum := hmac.New(sha256.New, secret)
	_, _ = sum.Write(signed.body)
	wantSignature := "sha256=" + hex.EncodeToString(sum.Sum(nil))
	if signed.signature != wantSignature || !bytes.Equal(signed.body, body) || signed.contentType != "application/json" {
		t.Fatalf("received signature %q body %q type %q", signed.signature, signed.body, signed.contentType)
	}

	if err := sender.Send(t.Context(), destination, nil, body); err != nil {
		t.Fatal(err)
	}
	unsigned := <-received
	if unsigned.signature != "" {
		t.Fatalf("unsigned webhook had signature %q", unsigned.signature)
	}

	var secondRequests atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondRequests.Add(1)
	}))
	defer second.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", second.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	err := sender.Send(t.Context(), Destination{URL: redirect.URL}, nil, body)
	if SendErrorCode(err) != redirectCode {
		t.Fatalf("redirect error = %v, code %q", err, SendErrorCode(err))
	}
	if got := secondRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}

func TestEmailMessage(t *testing.T) {
	config, _ := json.Marshal(map[string]any{
		"from": "Équipe <alerts@olp.test>",
		"to":   []string{"Ops <ops@olp.test>"},
	})
	opened, err := open(testPolicy(), Destination{
		Type: emailType, URL: "smtps://127.0.0.1:25", Configuration: config,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	channel := opened.(email)
	value := strings.Repeat("x", 3000)
	body, err := json.Marshal(map[string]string{"event": "large", "evidence": value})
	if err != nil {
		t.Fatal(err)
	}
	message := channel.message(body, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	header, encodedBody, ok := strings.Cut(string(message), "\r\n\r\n")
	if !ok {
		t.Fatal("message has no body")
	}
	if !strings.Contains(header, "From: =?") {
		t.Fatalf("From was not RFC 2047 encoded: %q", header)
	}
	if strings.IndexFunc(header, func(r rune) bool { return r > 127 }) >= 0 {
		t.Fatalf("header contains raw non-ASCII bytes: %q", header)
	}
	if !strings.Contains(header, "Content-Transfer-Encoding: quoted-printable") {
		t.Fatalf("quoted-printable header missing: %q", header)
	}
	var messageID string
	for _, line := range strings.Split(header, "\r\n") {
		if strings.HasPrefix(line, "Message-ID:") {
			messageID = strings.TrimPrefix(line, "Message-ID: ")
		}
	}
	if !strings.HasSuffix(messageID, "@olp.test>") {
		t.Fatalf("Message-ID = %q, want From domain", messageID)
	}
	for _, line := range strings.Split(string(message), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("message line has %d bytes", len(line))
		}
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(encodedBody)))
	if err != nil || !strings.Contains(string(decoded), value) {
		t.Fatalf("decoded body does not contain evidence: err=%v", err)
	}
}
