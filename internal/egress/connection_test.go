package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func connectionPointer[T any](value T) *T { return &value }

func loopbackConnections() Policy {
	return Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}, PlainHTTPHosts: []string{"127.0.0.1", "localhost"}}
}

type connectionTestCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         string
}

func newConnectionTestCA(t *testing.T) connectionTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local connection test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return connectionTestCA{certificate: certificate, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}
func (ca connectionTestCA) identity(t *testing.T, serial int64, client bool) (tls.Certificate, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "local connection test"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	certificate, err := tls.X509KeyPair([]byte(certificatePEM), []byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	return certificate, certificatePEM, keyPEM
}
func tlsConnectionServer(t *testing.T, ca connectionTestCA, clientAuth bool, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	certificate, _, _ := ca.identity(t, 2, false)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	if clientAuth {
		roots := x509.NewCertPool()
		roots.AddCert(ca.certificate)
		server.TLS.ClientAuth = tls.RequireAndVerifyClientCert
		server.TLS.ClientCAs = roots
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}
func networkSecret(t *testing.T, values map[string]string) []byte {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
func connectionResponse(t *testing.T, client *http.Client, endpoint string) string {
	t.Helper()
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	return string(data)
}

func TestConnectionTrustRootsAndMutualTLS(t *testing.T) {
	ca := newConnectionTestCA(t)
	var peer atomic.Int64
	server := tlsConnectionServer(t, ca, true, func(w http.ResponseWriter, r *http.Request) {
		peer.Store(r.TLS.PeerCertificates[0].SerialNumber.Int64())
		if r.TLS.ServerName != "localhost" {
			t.Errorf("target TLS hostname = %q", r.TLS.ServerName)
		}
		io.WriteString(w, "trusted")
	})
	endpoint := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	_, cert, key := ca.identity(t, 3, true)
	secret := networkSecret(t, map[string]string{"client_certificate_pem": cert, "client_key_pem": key})
	options := &ConnectionOptions{TrustRootsPEM: ca.pem, CredentialID: "network-credential"}
	client, err := loopbackConnections().ConnectionClient(options, secret, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if got := connectionResponse(t, client, endpoint); got != "trusted" || peer.Load() != 3 {
		t.Fatal("client certificate was not used")
	}
	withoutTrust := *options
	withoutTrust.TrustRootsPEM = ""
	untrusted, err := loopbackConnections().ConnectionClient(&withoutTrust, secret, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.CloseIdleConnections()
	if _, err = untrusted.Get(endpoint); err == nil {
		t.Fatal("private CA was trusted without configuration")
	}
	withoutCertificate, err := loopbackConnections().ConnectionClient(&ConnectionOptions{TrustRootsPEM: ca.pem}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer withoutCertificate.CloseIdleConnections()
	if _, err = withoutCertificate.Get(endpoint); err == nil {
		t.Fatal("mutual TLS succeeded without a client certificate")
	}
	_, _, wrongKey := ca.identity(t, 4, true)
	if err = ValidateConnectionSecret(networkSecret(t, map[string]string{"client_certificate_pem": cert, "client_key_pem": wrongKey})); err == nil || strings.Contains(err.Error(), wrongKey) {
		t.Fatal("mismatched client key accepted or exposed")
	}
	if err = loopbackConnections().ValidateConnection(&ConnectionOptions{TrustRootsPEM: key}); err == nil {
		t.Fatal("private key accepted in public trust roots")
	}
}

type connectObservation struct {
	target, authentication  string
	proxySNI                string
	proxyClientCertificates int
}

func connectProxy(t *testing.T, ca connectionTestCA, secure bool) (*httptest.Server, <-chan connectObservation) {
	t.Helper()
	observations := make(chan connectObservation, 16)
	handler := func(w http.ResponseWriter, r *http.Request) {
		observation := connectObservation{target: r.Host, authentication: r.Header.Get("Proxy-Authorization")}
		if r.TLS != nil {
			observation.proxySNI = r.TLS.ServerName
			observation.proxyClientCertificates = len(r.TLS.PeerCertificates)
		}
		observations <- observation
		if r.Method != http.MethodConnect {
			t.Error("HTTP proxy did not receive CONNECT")
			http.Error(w, "CONNECT required", 405)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			t.Error(err)
			http.Error(w, "bad target", 400)
			return
		}
		if _, err = netip.ParseAddr(host); err != nil {
			t.Error("proxy received remote-DNS target", host)
			http.Error(w, "IP required", 400)
			return
		}
		destination, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "connect failed", 502)
			return
		}
		defer destination.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if buffered.Flush() != nil {
			return
		}
		copied := make(chan struct{})
		go func() { _, _ = io.Copy(destination, buffered); _ = destination.Close(); close(copied) }()
		_, _ = io.Copy(client, destination)
		_ = client.Close()
		<-copied
	}
	var server *httptest.Server
	if secure {
		certificate, _, _ := ca.identity(t, 5, false)
		server = httptest.NewUnstartedServer(http.HandlerFunc(handler))
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequestClientCert}
		server.StartTLS()
	} else {
		server = httptest.NewServer(http.HandlerFunc(handler))
	}
	t.Cleanup(server.Close)
	return server, observations
}

func TestConnectionHTTPProxiesPinIPAndKeepBothTLSIdentities(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(strconv.FormatBool(secure), func(t *testing.T) {
			ca := newConnectionTestCA(t)
			target := tlsConnectionServer(t, ca, true, func(w http.ResponseWriter, r *http.Request) {
				if r.TLS.ServerName != "localhost" || !strings.HasPrefix(r.Host, "localhost:") {
					t.Errorf("provider hostname changed: SNI=%q Host=%q", r.TLS.ServerName, r.Host)
				}
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked to provider")
				}
				if r.URL.Query().Get("version") != "fixture" {
					t.Error("provider query was lost")
				}
				io.WriteString(w, "tunneled")
			})
			proxy, observations := connectProxy(t, ca, secure)
			_, cert, key := ca.identity(t, 6, true)
			secret := networkSecret(t, map[string]string{"proxy_username": "network-user", "proxy_password": "private-proxy-password", "client_certificate_pem": cert, "client_key_pem": key})
			client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL, TrustRootsPEM: ca.pem, CredentialID: "network-ref"}, secret, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			endpoint := strings.Replace(target.URL, "127.0.0.1", "localhost", 1) + "/path?version=fixture"
			if got := connectionResponse(t, client, endpoint); got != "tunneled" {
				t.Fatal(got)
			}
			if len(observations) == 0 {
				t.Fatal("configured proxy was bypassed")
			}
			for len(observations) > 0 {
				observation := <-observations
				if observation.authentication != "Basic bmV0d29yay11c2VyOnByaXZhdGUtcHJveHktcGFzc3dvcmQ=" {
					t.Fatal("proxy authentication missing")
				}
				if observation.proxyClientCertificates != 0 {
					t.Fatal("provider client certificate was offered to proxy")
				}
				if secure && observation.proxySNI != "" {
					t.Fatal("target SNI leaked to IP-addressed proxy")
				}
			}
		})
	}
}

func TestConnectionProxyCannotResolveOrReachUnapprovedDestination(t *testing.T) {
	var calls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 500) }))
	defer proxy.Close()
	policy := Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	for _, scheme := range []string{"http", "socks5"} {
		endpoint := strings.Replace(proxy.URL, "http://", scheme+"://", 1)
		client, err := policy.ConnectionClient(&ConnectionOptions{ProxyURL: endpoint}, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, destination := range []string{"https://127.0.0.2/", "https://169.254.169.254/latest/meta-data/"} {
			if _, err = client.Get(destination); !errors.Is(err, ErrUnsafeDestination) {
				t.Fatalf("private destination via %s: %v", scheme, err)
			}
		}
		client.CloseIdleConnections()
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe destination reached a proxy")
	}
	for _, endpoint := range []string{"socks5h://127.0.0.1:1080", "http://user:secret@127.0.0.1:8080", "https://169.254.169.254:443", "http://127.0.0.1:8080/path", "http://127.0.0.1:8080?target=secret"} {
		if err := policy.ValidateConnection(&ConnectionOptions{ProxyURL: endpoint}); err == nil {
			t.Fatal("unsafe proxy configuration accepted", endpoint)
		}
	}
}

type socksObservation struct {
	target, username, password string
	addressType                byte
}

func socksProxy(t *testing.T) (string, <-chan socksObservation) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	observations := make(chan socksObservation, 16)
	closed := make(chan struct{})
	var workers sync.WaitGroup
	go func() {
		defer close(closed)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer client.Close()
				_ = client.SetDeadline(time.Now().Add(5 * time.Second))
				var hello [3]byte
				if _, err := io.ReadFull(client, hello[:]); err != nil {
					return
				}
				if hello[0] != 5 || hello[1] != 1 {
					t.Error("invalid SOCKS5 greeting")
					return
				}
				method := hello[2]
				if method != 0 && method != 2 {
					t.Error("invalid SOCKS5 authentication method")
					return
				}
				if _, err := client.Write([]byte{5, method}); err != nil {
					return
				}
				observation := socksObservation{}
				if method == 2 {
					var header [2]byte
					if _, err := io.ReadFull(client, header[:]); err != nil {
						return
					}
					if header[0] != 1 {
						t.Error("invalid auth version")
						return
					}
					username := make([]byte, int(header[1]))
					if _, err := io.ReadFull(client, username); err != nil {
						return
					}
					if _, err := io.ReadFull(client, header[:1]); err != nil {
						return
					}
					password := make([]byte, int(header[0]))
					if _, err := io.ReadFull(client, password); err != nil {
						return
					}
					observation.username = string(username)
					observation.password = string(password)
					if _, err := client.Write([]byte{1, 0}); err != nil {
						return
					}
				}
				var request [4]byte
				if _, err := io.ReadFull(client, request[:]); err != nil {
					return
				}
				if request[0] != 5 || request[1] != 1 || request[2] != 0 {
					t.Error("invalid SOCKS5 request")
					return
				}
				observation.addressType = request[3]
				length := 0
				switch request[3] {
				case 1:
					length = 4
				case 4:
					length = 16
				default:
					t.Error("SOCKS5 received a remotely resolved hostname")
					return
				}
				address := make([]byte, length+2)
				if _, err := io.ReadFull(client, address); err != nil {
					return
				}
				observation.target = net.JoinHostPort(net.IP(address[:length]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(address[length:]))))
				observations <- observation
				destination, err := net.DialTimeout("tcp", observation.target, time.Second)
				if err != nil {
					_, _ = client.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer destination.Close()
				if _, err := client.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
					return
				}
				_ = client.SetDeadline(time.Time{})
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(destination, client); _ = destination.Close(); close(copied) }()
				_, _ = io.Copy(client, destination)
				_ = client.Close()
				<-copied
			})
		}
	}()
	t.Cleanup(func() { listener.Close(); <-closed; workers.Wait() })
	return "socks5://" + listener.Addr().String(), observations
}

func TestConnectionSOCKS5PinsAddressAndAuthenticates(t *testing.T) {
	ca := newConnectionTestCA(t)
	server := tlsConnectionServer(t, ca, false, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "localhost" {
			t.Error("SOCKS lost target TLS identity")
		}
		io.WriteString(w, "socks")
	})
	proxy, observations := socksProxy(t)
	secret := networkSecret(t, map[string]string{"proxy_username": "fixture-user", "proxy_password": "fixture-password"})
	client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy, TrustRootsPEM: ca.pem, CredentialID: "network-ref"}, secret, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if got := connectionResponse(t, client, strings.Replace(server.URL, "127.0.0.1", "localhost", 1)); got != "socks" {
		t.Fatal(got)
	}
	if len(observations) == 0 {
		t.Fatal("SOCKS5 proxy was bypassed")
	}
	for len(observations) > 0 {
		observation := <-observations
		if observation.username != "fixture-user" || observation.password != "fixture-password" || observation.addressType == 3 {
			t.Fatal("wrong SOCKS5 authentication or DNS mode")
		}
	}
}

func TestConnectionCustomTLSKeepsHTTP2Negotiation(t *testing.T) {
	ca := newConnectionTestCA(t)
	certificate, _, _ := ca.identity(t, 7, false)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("provider protocol = %s", r.Proto)
		}
		io.WriteString(w, "http2")
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{TrustRootsPEM: ca.pem}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if got := connectionResponse(t, client, server.URL); got != "http2" {
		t.Fatal(got)
	}
}

func TestConnectionKeepsDuplexUpgradeWritableAcrossCacheEviction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "fixture-duplex" {
			http.Error(w, "upgrade required", 400)
			return
		}
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fixture-duplex\r\n\r\n")
		if buffered.Flush() != nil {
			return
		}
		var input [4]byte
		if _, err = io.ReadFull(buffered, input[:]); err != nil {
			t.Error(err)
			return
		}
		if string(input[:]) != "ping" {
			t.Error("duplex input changed")
		}
		_, _ = connection.Write([]byte("pong"))
	}))
	defer server.Close()
	cache := NewConnectionClientCache(1)
	defer cache.CloseIdleConnections()
	client, err := cache.Client(loopbackConnections(), nil, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "fixture-duplex")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	duplex, ok := response.Body.(io.ReadWriteCloser)
	if !ok || response.StatusCode != 101 {
		t.Fatal("upgraded transport is not duplex")
	}
	if _, err = cache.Client(loopbackConnections(), &ConnectionOptions{MaxIdleConns: connectionPointer(8)}, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = duplex.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var output [4]byte
	if _, err = io.ReadFull(duplex, output[:]); err != nil {
		t.Fatal(err)
	}
	if string(output[:]) != "pong" {
		t.Fatal("duplex output changed")
	}
}
