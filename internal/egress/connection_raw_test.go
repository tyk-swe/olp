package egress

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRawConnectionHeadersSurviveCloseAndInformationalResponses(t *testing.T) {
	for _, mode := range []string{"HTTP", "TLS", "TLS through proxy"} {
		t.Run(mode, func(t *testing.T) {
			wire := "native body\x00\n\nConnection: body-is-not-a-header"
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Connection", "X-Informational-Hop")
				w.WriteHeader(http.StatusEarlyHints)
				w.Header()["Connection"] = []string{"close, X-Response-Hop", "X-Other-Hop"}
				w.Header().Set("Trailer", "X-Native-Trailer, X-Response-Hop")
				_, _ = io.WriteString(w, wire)
				w.Header()["X-Native-Trailer"] = []string{"one", "two"}
				w.Header().Set("X-Response-Hop", "hop trailer")
			})
			var upstream *httptest.Server
			options := &ConnectionOptions{}
			if mode == "HTTP" {
				upstream = httptest.NewServer(handler)
				t.Cleanup(upstream.Close)
			} else {
				ca := newConnectionTestCA(t)
				upstream = tlsConnectionServer(t, ca, false, handler)
				options.TrustRootsPEM = ca.pem
				if mode == "TLS through proxy" {
					proxy, _ := connectProxy(t, ca, true)
					options.ProxyURL = proxy.URL
				}
			}
			client, err := loopbackConnections().RawConnectionClient(options, nil, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			response, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != wire || response.StatusCode != http.StatusOK || !response.Close {
				t.Fatalf("native response changed: status=%d close=%v body=%q: %v", response.StatusCode, response.Close, body, err)
			}
			if !reflect.DeepEqual(response.Header.Values("Connection"), []string{"close, X-Response-Hop", "X-Other-Hop"}) {
				t.Fatalf("final connection values lost: %v", response.Header.Values("Connection"))
			}
			if !reflect.DeepEqual(response.Trailer.Values("X-Native-Trailer"), []string{"one", "two"}) || response.Trailer.Get("X-Response-Hop") != "hop trailer" {
				t.Fatalf("raw trailers changed: %v", response.Trailer)
			}
			if mode != "HTTP" && (response.TLS == nil || len(response.TLS.VerifiedChains) == 0) {
				t.Fatal("verified target TLS state lost")
			}
		})
	}
}

func TestRawConnectionCapturesFragmentedHeadersWithoutChangingBytes(t *testing.T) {
	wire := "HTTP/1.1 100 Continue\r\nConnection: X-Continue\r\n\r\n" +
		"HTTP/1.1 103 Early Hints\nConnection: X-Hint\n\n" +
		"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade,\r\n X-Upgrade-Hop\r\nConnection: close\r\n\r\n" +
		"\x00\xff\n\nConnection: body-is-not-a-header"
	reader, writer := net.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		defer writer.Close()
		for _, value := range []byte(wire) {
			if _, err := writer.Write([]byte{value}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	connection := &connectionHeaderConn{Conn: reader}
	body, err := io.ReadAll(connection)
	if err != nil || string(body) != wire || <-done != nil {
		t.Fatalf("fragmented bytes changed: %q: %v", body, err)
	}
	if strings.Join(connection.connections, "|") != "Upgrade, X-Upgrade-Hop|close" {
		t.Fatalf("final header values changed: %v", connection.connections)
	}
}
