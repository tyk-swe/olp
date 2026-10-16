package notifications

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
	"net/netip"
	"net/url"
	"strings"
	"sync"
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

// A cancelled handshake must have closed the raw connection, so draining
// it (including any buffered TLS bytes) must end in EOF, never a timeout.

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
