package egress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionOptionsAndSecretsRejectUnsafeOrUnboundedValues(t *testing.T) {
	for _, options := range []*ConnectionOptions{
		{ConnectTimeoutMS: connectionPointer(int64(0))}, {ConnectTimeoutMS: connectionPointer(int64(120001))},
		{TLSHandshakeTimeoutMS: connectionPointer(int64(-1))}, {TLSHandshakeTimeoutMS: connectionPointer(int64(120001))},
		{ResponseHeaderTimeoutMS: connectionPointer(int64(600001))}, {IdleConnTimeoutMS: connectionPointer(int64(3600001))},
		{MaxIdleConns: connectionPointer(0)}, {MaxIdleConnsPerHost: connectionPointer(4097)}, {MaxConnsPerHost: connectionPointer(-1)},
		{MaxIdleConns: connectionPointer(4), MaxIdleConnsPerHost: connectionPointer(5)},
		{MaxConnsPerHost: connectionPointer(4), MaxIdleConnsPerHost: connectionPointer(5)},
		{TrustRootsPEM: "not a certificate"}, {CredentialID: "secret\nreference"},
	} {
		if err := loopbackConnections().ValidateConnection(options); err == nil {
			t.Fatal("unsafe connection options accepted")
		}
	}
	for _, secret := range []string{"", `null`, `{}`, `{"unknown":"private-value"}`, `{"proxy_username":null}`, `{"proxy_username":"first","proxy_username":"second"}`, `{"proxy_password":"private-value"}`, `{"proxy_username":"user:name","proxy_password":"private-value"}`, `{"client_key_pem":"private-value"}`, `{"client_certificate_pem":"private-value","client_key_pem":"private-value"}`} {
		if err := ValidateConnectionSecret([]byte(secret)); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("invalid network credential accepted or exposed")
		}
	}
	if _, err := loopbackConnections().ConnectionClient(&ConnectionOptions{CredentialID: "unavailable"}, nil, time.Second); err == nil {
		t.Fatal("missing referenced credential downgraded to anonymous")
	}
	secret := networkSecret(t, map[string]string{"proxy_username": "user", "proxy_password": "password"})
	if _, err := loopbackConnections().ConnectionClient(nil, secret, time.Second); err == nil {
		t.Fatal("proxy credentials accepted without proxy")
	}
	longSecret := networkSecret(t, map[string]string{"proxy_username": "user", "proxy_password": strings.Repeat("p", 512)})
	if err := ValidateConnectionSecret(longSecret); err != nil {
		t.Fatal("valid HTTP proxy credential was constrained by SOCKS framing", err)
	}
	if _, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: "socks5://127.0.0.1:1080", CredentialID: "network-reference"}, longSecret, time.Second); err == nil {
		t.Fatal("oversized SOCKS credential accepted")
	}
}

func TestConnectionCacheReusesAndBoundsIdlePools(t *testing.T) {
	var accepted atomic.Int64
	closed := make(chan struct{}, 16)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pooled") }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	cache := NewConnectionClientCache(2)
	defer cache.CloseIdleConnections()
	get := func(timeout int64) {
		t.Helper()
		client, err := cache.Client(loopbackConnections(), &ConnectionOptions{ResponseHeaderTimeoutMS: &timeout}, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := connectionResponse(t, client, server.URL); got != "pooled" {
			t.Fatal(got)
		}
	}
	for range 5 {
		get(1000)
	}
	if accepted.Load() != 1 {
		t.Fatalf("identical configuration created %d connections", accepted.Load())
	}
	get(2000)
	get(3000)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("eviction did not close the retired idle pool")
	}
	get(1000)
	if accepted.Load() != 4 {
		t.Fatalf("evicted configuration reused a retired pool: %d accepts", accepted.Load())
	}
	cache.CloseIdleConnections()
	get(1000)
	if accepted.Load() != 5 {
		t.Fatal("cache close retained an idle connection")
	}
}

func TestConnectionCacheSeparatesRotatedNetworkIdentity(t *testing.T) {
	ca := newConnectionTestCA(t)
	var observed atomic.Int64
	server := tlsConnectionServer(t, ca, true, func(w http.ResponseWriter, r *http.Request) {
		observed.Store(r.TLS.PeerCertificates[0].SerialNumber.Int64())
		io.WriteString(w, "identity")
	})
	cache := NewConnectionClientCache(4)
	defer cache.CloseIdleConnections()
	options := &ConnectionOptions{CredentialID: "same-opaque-reference", TrustRootsPEM: ca.pem}
	for _, serial := range []int64{31, 32, 31} {
		_, certificate, key := ca.identity(t, serial, true)
		secret := networkSecret(t, map[string]string{"client_certificate_pem": certificate, "client_key_pem": key})
		client, err := cache.Client(loopbackConnections(), options, secret, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connectionResponse(t, client, strings.Replace(server.URL, "127.0.0.1", "localhost", 1))
		if observed.Load() != serial {
			t.Fatal("network secret rotation reused the wrong authenticated TLS connection")
		}
	}
}

func TestConnectionPhaseTimeoutsAndCancellation(t *testing.T) {
	t.Run("response-headers", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ResponseHeaderTimeoutMS: connectionPointer(int64(60))}, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		started := time.Now()
		_, err = client.Get(server.URL)
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(started) > 2*time.Second {
			t.Fatalf("response header deadline failed: %v", err)
		}
	})
	t.Run("target-TLS-handshake", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		connection := make(chan net.Conn, 1)
		go func() {
			socket, err := listener.Accept()
			if err == nil {
				connection <- socket
			}
		}()
		client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{TLSHandshakeTimeoutMS: connectionPointer(int64(60))}, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		started := time.Now()
		_, err = client.Get("https://" + listener.Addr().String())
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(started) > 2*time.Second {
			t.Fatalf("TLS deadline failed: %v", err)
		}
		select {
		case socket := <-connection:
			socket.Close()
		case <-time.After(time.Second):
			t.Fatal("TLS fixture did not accept")
		}
	})
	for _, cancelRequest := range []bool{false, true} {
		name := "proxy-connect-timeout"
		if cancelRequest {
			name = "proxy-connect-cancellation"
		}
		t.Run(name, func(t *testing.T) {
			arrived := make(chan struct{}, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { arrived <- struct{}{}; <-r.Context().Done() }))
			defer proxy.Close()
			timeout := int64(60)
			if cancelRequest {
				timeout = 5000
			}
			client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL, ConnectTimeoutMS: &timeout}, nil, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:443/", nil)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				response, err := client.Do(request)
				if response != nil {
					response.Body.Close()
				}
				result <- err
			}()
			select {
			case <-arrived:
			case <-time.After(time.Second):
				t.Fatal("proxy did not receive CONNECT")
			}
			if cancelRequest {
				cancel()
			}
			select {
			case err := <-result:
				expected := context.DeadlineExceeded
				if cancelRequest {
					expected = context.Canceled
				}
				if !errors.Is(err, expected) {
					t.Fatalf("phase cancellation/deadline: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("proxy negotiation ignored cancellation or deadline")
			}
		})
	}
}

func TestConnectionPoolLimitCancelsWaitingRequests(t *testing.T) {
	started := make(chan struct{}, 4)
	var active, maximum atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{MaxConnsPerHost: connectionPointer(2)}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 4)
	for range 4 {
		go func() {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			response, err := client.Do(request)
			if response != nil {
				response.Body.Close()
			}
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("pool requests did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("per-host pool bound exceeded")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	for range 4 {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiting request failed cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("queued request leaked")
		}
	}
	if maximum.Load() != 2 {
		t.Fatal("wrong connection concurrency", maximum.Load())
	}
}

func TestConnectionRejectsRedirectsEnvironmentProxiesAndOversizedProxyHeaders(t *testing.T) {
	var unintended atomic.Int64
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unintended.Add(1)
		http.Error(w, "unexpected proxy", 500)
	}))
	defer trap.Close()
	t.Setenv("HTTP_PROXY", trap.URL)
	t.Setenv("HTTPS_PROXY", trap.URL)
	t.Setenv("ALL_PROXY", trap.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://169.254.169.254/", 302)
			return
		}
		io.WriteString(w, "direct")
	}))
	defer server.Close()
	client, err := loopbackConnections().ConnectionClient(nil, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if got := connectionResponse(t, client, server.URL); got != "direct" {
		t.Fatal(got)
	}
	if _, err = client.Get(server.URL + "/redirect"); !errors.Is(err, ErrRedirect) {
		t.Fatalf("redirect not refused: %v", err)
	}
	if unintended.Load() != 0 {
		t.Fatal("environment proxy was used")
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", 40<<10))
		w.WriteHeader(200)
	}))
	defer proxy.Close()
	bounded, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer bounded.CloseIdleConnections()
	if _, err = bounded.Get(server.URL); err == nil || !strings.Contains(err.Error(), "32 KiB") {
		t.Fatalf("unbounded CONNECT headers: %v", err)
	}
}

func TestConnectionOptionsRoundTripDoesNotContainSecret(t *testing.T) {
	options := ConnectionOptions{ProxyURL: "https://proxy.example:443", CredentialID: "opaque-reference", ConnectTimeoutMS: connectionPointer(int64(3000)), TLSHandshakeTimeoutMS: connectionPointer(int64(4000)), ResponseHeaderTimeoutMS: connectionPointer(int64(5000)), IdleConnTimeoutMS: connectionPointer(int64(6000)), MaxIdleConns: connectionPointer(32), MaxIdleConnsPerHost: connectionPointer(8), MaxConnsPerHost: connectionPointer(16)}
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ConnectionOptions
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = (Policy{}).ValidateConnection(&decoded); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"proxy_password", "client_key_pem", "client_certificate_pem"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("plaintext credential field appeared in configuration")
		}
	}
}

func TestConnectionSOCKSNegotiationHonorsCancellationAndRejectsAuthDowngrade(t *testing.T) {
	for _, downgrade := range []bool{false, true} {
		name := "cancellation"
		if downgrade {
			name = "authentication-downgrade"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			greeting := make(chan struct{})
			finished := make(chan struct{})
			var sentAfterGreeting atomic.Int64
			go func() {
				defer close(finished)
				socket, err := listener.Accept()
				if err != nil {
					return
				}
				defer socket.Close()
				_ = socket.SetDeadline(time.Now().Add(2 * time.Second))
				var header [3]byte
				if _, err := io.ReadFull(socket, header[:]); err != nil {
					return
				}
				if header != [3]byte{5, 1, 2} {
					t.Error("proxy authentication was not required")
				}
				close(greeting)
				if downgrade {
					_, _ = socket.Write([]byte{5, 0})
				}
				n, _ := socket.Read(header[:])
				sentAfterGreeting.Store(int64(n))
			}()
			secret := networkSecret(t, map[string]string{"proxy_username": "private-user", "proxy_password": "private-password"})
			client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: "socks5://" + listener.Addr().String(), CredentialID: "secret-reference"}, secret, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1/", nil)
			result := make(chan error, 1)
			go func() {
				response, err := client.Do(request)
				if response != nil {
					response.Body.Close()
				}
				result <- err
			}()
			select {
			case <-greeting:
			case <-time.After(time.Second):
				t.Fatal("proxy greeting did not arrive")
			}
			if !downgrade {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil || strings.Contains(err.Error(), "private-password") {
					t.Fatal("SOCKS negotiation succeeded or exposed credentials")
				}
				if !downgrade && !errors.Is(err, context.Canceled) {
					t.Fatalf("SOCKS negotiation ignored cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("SOCKS negotiation did not stop")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("SOCKS socket leaked after failure")
			}
			if sentAfterGreeting.Load() != 0 {
				t.Fatal("SOCKS sent credentials or target after failed negotiation")
			}
		})
	}
}

func TestConnectionHTTPSProxyRequiresItsOwnTrustRoot(t *testing.T) {
	targetCA := newConnectionTestCA(t)
	proxyCA := newConnectionTestCA(t)
	var targetCalls atomic.Int64
	target := tlsConnectionServer(t, targetCA, false, func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); io.WriteString(w, "two-roots") })
	proxy, observations := connectProxy(t, proxyCA, true)
	client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL, TrustRootsPEM: targetCA.pem}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if _, err = client.Get(target.URL); err == nil {
		t.Fatal("untrusted HTTPS proxy certificate was accepted")
	}
	if targetCalls.Load() != 0 || len(observations) != 0 {
		t.Fatal("untrusted proxy received CONNECT or provider work")
	}
	trusted, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL, TrustRootsPEM: targetCA.pem + proxyCA.pem}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.CloseIdleConnections()
	if got := connectionResponse(t, trusted, target.URL); got != "two-roots" {
		t.Fatal(got)
	}
}

func TestConnectionEvictionDoesNotRetainLateOrActiveIdlePools(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "late-client-handle"
		if active {
			name = "active-response"
		}
		t.Run(name, func(t *testing.T) {
			closed := make(chan struct{}, 4)
			release := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if active {
					<-release
				}
				io.WriteString(w, "complete")
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			server.Start()
			defer server.Close()
			cache := NewConnectionClientCache(1)
			defer cache.CloseIdleConnections()
			old, err := cache.Client(loopbackConnections(), nil, nil, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var response *http.Response
			if active {
				response, err = old.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = cache.Client(loopbackConnections(), &ConnectionOptions{MaxIdleConns: connectionPointer(8)}, nil, time.Second); err != nil {
				t.Fatal(err)
			}
			if active {
				close(release)
				_, err = io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				connectionResponse(t, old, server.URL)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("retired transport retained an idle connection after a late or in-flight request")
			}
		})
	}
}
