package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
)

func loopbackPolicy() egress.Policy {
	return egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

func openDestination(typ contract.NotificationDestinationType, rawURL, configuration, secret string) error {
	policy := loopbackPolicy()
	_, err := open(&policy, destination{Type: typ, URL: rawURL, Configuration: json.RawMessage(configuration)}, []byte(secret))
	return err
}

func sendDestination(ctx context.Context, t *testing.T, typ contract.NotificationDestinationType, rawURL, secret string) Failure {
	t.Helper()
	m, err := newMessage([]byte(`{"version":1,"event":"worker.stale","evidence":{"task":"usage"}}`),
		contract.NotificationEventWorkerStale, "stale workers", "olp:incident:1", false)
	if err != nil {
		t.Fatal(err)
	}
	return sendErrorCode(NewSender(loopbackPolicy()).send(ctx, destination{Type: typ, URL: rawURL}, []byte(secret), m))
}

type openCase struct {
	name, url, configuration, secret string
	ok                               bool
}

func TestOpenWebhookDestination(t *testing.T) {
	t.Parallel()
	for _, test := range []openCase{
		{name: "https", url: "https://hooks.example.com/olp", ok: true},
		{name: "signed", url: "https://hooks.example.com/olp", secret: "signing-key", ok: true},
		{name: "empty configuration object", url: "https://hooks.example.com/olp", configuration: `{}`, ok: true},
		{name: "plain http on allowed host", url: "http://127.0.0.1:8080/olp", ok: true},
		{name: "plain http elsewhere", url: "http://hooks.example.com/olp"},
		{name: "query", url: "https://hooks.example.com/olp?token=1"},
		{name: "fragment", url: "https://hooks.example.com/olp#section"},
		{name: "userinfo", url: "https://user:pass@hooks.example.com/olp"},
		{name: "blocked network", url: "https://10.0.0.1/olp"},
		{name: "surrounding whitespace", url: " https://hooks.example.com/olp"},
		{name: "configuration fields", url: "https://hooks.example.com/olp", configuration: `{"channel":"ops"}`},
		{name: "configuration not an object", url: "https://hooks.example.com/olp", configuration: `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := openDestination(contract.NotificationDestinationTypeWebhook, test.url, test.configuration, test.secret); (err == nil) != test.ok {
				t.Fatalf("open() error = %v, want ok %t", err, test.ok)
			}
		})
	}
}

func TestOpenChatDestination(t *testing.T) {
	t.Parallel()
	const hook = `{"webhook_url":"https://hooks.example.com/services/T/B/x"}`
	cases := []openCase{
		{name: "bare origin", url: "https://hooks.example.com", secret: hook, ok: true},
		{name: "hook keeps query", url: "https://hooks.example.com", secret: `{"webhook_url":"https://hooks.example.com/services/x?sig=abc"}`, ok: true},
		{name: "plain http on allowed host", url: "http://127.0.0.1:9000", secret: `{"webhook_url":"http://127.0.0.1:9000/hook"}`, ok: true},
		{name: "origin with path", url: "https://hooks.example.com/services", secret: hook},
		{name: "origin with query", url: "https://hooks.example.com?team=ops", secret: hook},
		{name: "origin with fragment", url: "https://hooks.example.com#ops", secret: hook},
		{name: "origin with userinfo", url: "https://user@hooks.example.com", secret: hook},
		{name: "origin with surrounding whitespace", url: "https://hooks.example.com ", secret: hook},
		{name: "plain http elsewhere", url: "http://hooks.example.com", secret: `{"webhook_url":"http://hooks.example.com/services/x"}`},
		{name: "blocked network", url: "https://10.0.0.1", secret: `{"webhook_url":"https://10.0.0.1/hook"}`},
		{name: "configuration fields", url: "https://hooks.example.com", configuration: `{"channel":"#ops"}`, secret: hook},
		{name: "missing secret", url: "https://hooks.example.com"},
		{name: "secret string", url: "https://hooks.example.com", secret: `"https://hooks.example.com/services/x"`},
		{name: "secret key case", url: "https://hooks.example.com", secret: `{"WEBHOOK_URL":"https://hooks.example.com/services/x"}`},
		{name: "secret null value", url: "https://hooks.example.com", secret: `{"webhook_url":null}`},
		{name: "secret empty value", url: "https://hooks.example.com", secret: `{"webhook_url":""}`},
		{name: "secret extra key", url: "https://hooks.example.com", secret: `{"webhook_url":"https://hooks.example.com/services/x","channel":"#ops"}`},
		{name: "hook on another origin", url: "https://hooks.example.com", secret: `{"webhook_url":"https://other.example.com/services/x"}`},
		{name: "hook on another port", url: "https://hooks.example.com", secret: `{"webhook_url":"https://hooks.example.com:8443/services/x"}`},
		{name: "hook with another scheme", url: "https://hooks.example.com", secret: `{"webhook_url":"http://hooks.example.com/services/x"}`},
		{name: "hook with userinfo", url: "https://hooks.example.com", secret: `{"webhook_url":"https://user:pass@hooks.example.com/services/x"}`},
		{name: "hook too long", url: "https://hooks.example.com", secret: `{"webhook_url":"https://hooks.example.com/` + strings.Repeat("x", 2048) + `"}`},
		{name: "origin with default https port", url: "https://hooks.example.com:443", secret: hook, ok: true},
		{name: "hook with default https port", url: "https://hooks.example.com", secret: `{"webhook_url":"https://hooks.example.com:443/services/x"}`, ok: true},
		{name: "origin with default http port", url: "http://127.0.0.1:80", secret: `{"webhook_url":"http://127.0.0.1/hook"}`, ok: true},
		{name: "hook with default http port", url: "http://127.0.0.1", secret: `{"webhook_url":"http://127.0.0.1:80/hook"}`, ok: true},
		{name: "matching custom port", url: "https://hooks.example.com:8443", secret: `{"webhook_url":"https://hooks.example.com:8443/services/x"}`, ok: true},
		{name: "hook host case", url: "https://hooks.example.com", secret: `{"webhook_url":"https://HOOKS.example.com/services/x"}`, ok: true},
		{name: "hook without the custom port", url: "https://hooks.example.com:8443", secret: hook},
	}
	for _, typ := range []contract.NotificationDestinationType{contract.NotificationDestinationTypeSlack, contract.NotificationDestinationTypeMsteams, contract.NotificationDestinationTypeDiscord} {
		for _, test := range cases {
			t.Run(string(typ)+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				if err := openDestination(typ, test.url, test.configuration, test.secret); (err == nil) != test.ok {
					t.Fatalf("open() error = %v, want ok %t", err, test.ok)
				}
			})
		}
	}
}

func TestOpenPagerDutyDestination(t *testing.T) {
	t.Parallel()
	const target = "https://events.pagerduty.example/v2/enqueue"
	for _, test := range []openCase{
		{name: "routing key", url: target, secret: `{"routing_key":"R0UT1NG"}`, ok: true},
		{name: "missing secret", url: target},
		{name: "secret key case", url: target, secret: `{"ROUTING_KEY":"R0UT1NG"}`},
		{name: "secret null value", url: target, secret: `{"routing_key":null}`},
		{name: "secret empty value", url: target, secret: `{"routing_key":""}`},
		{name: "secret extra key", url: target, secret: `{"routing_key":"R0UT1NG","service":"ops"}`},
		{name: "secret too long", url: target, secret: `{"routing_key":"` + strings.Repeat("r", 513) + `"}`},
		{name: "query", url: target + "?region=eu", secret: `{"routing_key":"R0UT1NG"}`},
		{name: "plain http elsewhere", url: "http://events.pagerduty.example/v2/enqueue", secret: `{"routing_key":"R0UT1NG"}`},
		{name: "configuration fields", url: target, configuration: `{"severity":"critical"}`, secret: `{"routing_key":"R0UT1NG"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := openDestination(contract.NotificationDestinationTypePagerduty, test.url, test.configuration, test.secret); (err == nil) != test.ok {
				t.Fatalf("open() error = %v, want ok %t", err, test.ok)
			}
		})
	}
}

func TestOpenEmailDestination(t *testing.T) {
	t.Parallel()
	const (
		target      = "smtps://smtp.example.com:465"
		valid       = `{"from":"OLP <olp@example.com>","to":["ops@example.com"]}`
		credentials = `{"username":"olp","password":"hunter2"}`
	)
	recipients, err := json.Marshal(slices.Repeat([]string{"ops@example.com"}, 51))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []openCase{
		{name: "implicit tls", url: target, configuration: valid, ok: true},
		{name: "starttls with credentials", url: "smtp+starttls://smtp.example.com:587", configuration: `{"from":"olp@example.com","to":["ops@example.com","oncall@example.com"],"subject_prefix":"[OLP]"}`, secret: credentials, ok: true},
		{name: "empty credentials object", url: target, configuration: valid, secret: `{}`, ok: true},
		{name: "allowed loopback host", url: "smtps://127.0.0.1:465", configuration: valid, ok: true},
		{name: "plain smtp", url: "smtp://smtp.example.com:25", configuration: valid},
		{name: "https scheme", url: "https://smtp.example.com:465", configuration: valid},
		{name: "missing port", url: "smtps://smtp.example.com", configuration: valid},
		{name: "userinfo", url: "smtps://olp@smtp.example.com:465", configuration: valid},
		{name: "query", url: target + "?auth=plain", configuration: valid},
		{name: "fragment", url: target + "#relay", configuration: valid},
		{name: "path", url: target + "/relay", configuration: valid},
		{name: "blocked network", url: "smtps://10.0.0.1:465", configuration: valid},
		{name: "missing configuration", url: target},
		{name: "unknown field", url: target, configuration: `{"from":"olp@example.com","to":["ops@example.com"],"cc":["audit@example.com"]}`},
		{name: "null from", url: target, configuration: `{"from":null,"to":["ops@example.com"]}`},
		{name: "null to", url: target, configuration: `{"from":"olp@example.com","to":null}`},
		{name: "no recipients", url: target, configuration: `{"from":"olp@example.com","to":[]}`},
		{name: "too many recipients", url: target, configuration: `{"from":"olp@example.com","to":` + string(recipients) + `}`},
		{name: "invalid from address", url: target, configuration: `{"from":"not an address","to":["ops@example.com"]}`},
		{name: "invalid to address", url: target, configuration: `{"from":"olp@example.com","to":["ops"]}`},
		{name: "crlf in from", url: target, configuration: `{"from":"olp@example.com\r\nBcc: audit@example.com","to":["ops@example.com"]}`},
		{name: "crlf in to", url: target, configuration: `{"from":"olp@example.com","to":["ops@example.com\r\nBcc: audit@example.com"]}`},
		{name: "crlf in subject prefix", url: target, configuration: `{"from":"olp@example.com","to":["ops@example.com"],"subject_prefix":"[OLP]\r\nBcc: audit@example.com"}`},
		{name: "invalid ca certificate", url: target, configuration: `{"from":"olp@example.com","to":["ops@example.com"],"ca_certificate":"not pem"}`},
		{name: "username without password", url: target, configuration: valid, secret: `{"username":"olp"}`},
		{name: "password without username", url: target, configuration: valid, secret: `{"password":"hunter2"}`},
		{name: "empty password", url: target, configuration: valid, secret: `{"username":"olp","password":""}`},
		{name: "null username", url: target, configuration: valid, secret: `{"username":null,"password":"hunter2"}`},
		{name: "credential key case", url: target, configuration: valid, secret: `{"USERNAME":"olp","PASSWORD":"hunter2"}`},
		{name: "credentials extra key", url: target, configuration: valid, secret: `{"username":"olp","password":"hunter2","realm":"ops"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := openDestination(contract.NotificationDestinationTypeEmail, test.url, test.configuration, test.secret); (err == nil) != test.ok {
				t.Fatalf("open() error = %v, want ok %t", err, test.ok)
			}
		})
	}
}

func TestOpenRejectsUnsupportedType(t *testing.T) {
	t.Parallel()
	if err := openDestination("sms", "https://hooks.example.com/olp", "", ""); err == nil {
		t.Fatal("open() accepted an unsupported destination type")
	}
}

type capturedRequest struct {
	uri, contentType, signature string
	body                        []byte
}

func captureServer(t *testing.T, respond func(w http.ResponseWriter, body []byte)) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- capturedRequest{uri: r.RequestURI, contentType: r.Header.Get("Content-Type"), signature: r.Header.Get(egress.SignatureHeader), body: body}
		respond(w, body)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func noContent(w http.ResponseWriter, _ []byte) { w.WriteHeader(http.StatusNoContent) }

func TestWebhookSignsBodyWhenSecretIsSet(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, secret string
	}{{"signed", "signing-key"}, {"unsigned", ""}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, requests := captureServer(t, noContent)
			if code := sendDestination(t.Context(), t, contract.NotificationDestinationTypeWebhook, server.URL+"/olp", test.secret); code != "" {
				t.Fatalf("send() failure code %q", code)
			}
			got := <-requests
			want := make(http.Header)
			if test.secret != "" {
				egress.SignBody(want, []byte(test.secret), got.body)
			}
			if got.signature != want.Get(egress.SignatureHeader) {
				t.Errorf("signature = %q, want %q", got.signature, want.Get(egress.SignatureHeader))
			}
			if got.uri != "/olp" || got.contentType != "application/json" {
				t.Errorf("request = %s %s", got.uri, got.contentType)
			}
		})
	}
}

func TestChatPostsRenderedMessageToHook(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		typ        contract.NotificationDestinationType
		titleField string
		fields     []string
	}{
		{contract.NotificationDestinationTypeSlack, "text", []string{"blocks", "text"}},
		{contract.NotificationDestinationTypeMsteams, "type", []string{"attachments", "type"}},
		{contract.NotificationDestinationTypeDiscord, "content", []string{"allowed_mentions", "content", "embeds"}},
	} {
		t.Run(string(test.typ), func(t *testing.T) {
			t.Parallel()
			server, requests := captureServer(t, noContent)
			secret := `{"webhook_url":"` + server.URL + `/hooks/T/B?sig=abc"}`
			if code := sendDestination(t.Context(), t, test.typ, server.URL, secret); code != "" {
				t.Fatalf("send() failure code %q", code)
			}
			got := <-requests
			if got.uri != "/hooks/T/B?sig=abc" || got.contentType != "application/json" {
				t.Errorf("request = %s %s", got.uri, got.contentType)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(got.body, &body); err != nil {
				t.Fatal(err)
			}
			if keys := slices.Sorted(maps.Keys(body)); !slices.Equal(keys, test.fields) {
				t.Errorf("body fields = %v, want %v", keys, test.fields)
			}
			if test.typ != contract.NotificationDestinationTypeMsteams && !bytes.Contains(body[test.titleField], []byte("worker.stale")) {
				t.Errorf("%s = %s, want the event title", test.titleField, body[test.titleField])
			}
		})
	}
}

func TestPagerDutyRequiresMatchingAcknowledgement(t *testing.T) {
	t.Parallel()
	ack := func(status int, dedup func(sent string) string) func(http.ResponseWriter, []byte) {
		return func(w http.ResponseWriter, body []byte) {
			var event struct {
				DedupKey string `json:"dedup_key"`
			}
			_ = json.Unmarshal(body, &event)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "dedup_key": dedup(event.DedupKey)})
		}
	}
	same := func(sent string) string { return sent }
	other := func(string) string { return "olp:other:1" }
	for _, test := range []struct {
		name    string
		respond func(http.ResponseWriter, []byte)
		code    Failure
	}{
		{"accepted", ack(http.StatusAccepted, same), ""},
		{"dedup mismatch", ack(http.StatusAccepted, other), http5xxCode},
		{"success without 202", ack(http.StatusOK, same), http5xxCode},
		{"rejected", ack(http.StatusBadRequest, same), http4xxCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, requests := captureServer(t, test.respond)
			if code := sendDestination(t.Context(), t, contract.NotificationDestinationTypePagerduty, server.URL+"/v2/enqueue", `{"routing_key":"R0UT1NG"}`); code != test.code {
				t.Fatalf("send() failure code %q, want %q", code, test.code)
			}
			var event struct {
				RoutingKey  string `json:"routing_key"`
				EventAction string `json:"event_action"`
				DedupKey    string `json:"dedup_key"`
				Payload     struct {
					Summary       string          `json:"summary"`
					CustomDetails json.RawMessage `json:"custom_details"`
				} `json:"payload"`
			}
			if err := json.Unmarshal((<-requests).body, &event); err != nil {
				t.Fatal(err)
			}
			if event.RoutingKey != "R0UT1NG" || event.EventAction != "trigger" || event.DedupKey != "olp:incident:1" ||
				!strings.Contains(event.Payload.Summary, "worker.stale") || !bytes.Contains(event.Payload.CustomDetails, []byte(`"usage"`)) {
				t.Errorf("event = %+v", event)
			}
		})
	}
}

func TestSendMapsFailuresToCodes(t *testing.T) {
	t.Parallel()
	status := func(code int) func(http.ResponseWriter, []byte) {
		return func(w http.ResponseWriter, _ []byte) { w.WriteHeader(code) }
	}
	redirect := func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}
	for _, test := range []struct {
		name    string
		respond func(http.ResponseWriter, []byte)
		code    Failure
	}{
		{"bad request", status(http.StatusBadRequest), http4xxCode},
		{"too many requests", status(http.StatusTooManyRequests), http4xxCode},
		{"server error", status(http.StatusInternalServerError), http5xxCode},
		{"unavailable", status(http.StatusServiceUnavailable), http5xxCode},
		{"redirect", redirect, redirectCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, _ := captureServer(t, test.respond)
			if code := sendDestination(t.Context(), t, contract.NotificationDestinationTypeWebhook, server.URL+"/olp", ""); code != test.code {
				t.Fatalf("send() failure code %q, want %q", code, test.code)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if code := sendDestination(ctx, t, contract.NotificationDestinationTypeWebhook, server.URL+"/olp", ""); code != timeoutCode {
			t.Fatalf("send() failure code %q, want %q", code, timeoutCode)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		if code := sendDestination(t.Context(), t, contract.NotificationDestinationTypeWebhook, server.URL+"/olp", ""); code != networkCode {
			t.Fatalf("send() failure code %q, want %q", code, networkCode)
		}
	})
	t.Run("invalid destination", func(t *testing.T) {
		t.Parallel()
		if code := sendDestination(t.Context(), t, contract.NotificationDestinationTypeWebhook, "https://hooks.example.com/olp?token=1", ""); code != invalidDestinationCode {
			t.Fatalf("send() failure code %q, want %q", code, invalidDestinationCode)
		}
	})
}

func TestDestinationSecret(t *testing.T) {
	t.Parallel()
	longWebhook, err := json.Marshal(strings.Repeat("k", 1025))
	if err != nil {
		t.Fatal(err)
	}
	largeObject, err := json.Marshal(map[string]string{"credential": strings.Repeat("x", 16384)})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		typ     contract.NotificationDestinationType
		raw     json.RawMessage
		want    secretChange
		wantErr bool
	}{
		{name: "absent", typ: "slack", want: keptSecret{}},
		{name: "webhook string", typ: "webhook", raw: json.RawMessage(`"k"`), want: newSecret{[]byte("k")}},
		{name: "empty webhook string", typ: "webhook", raw: json.RawMessage(`""`), wantErr: true},
		{name: "long webhook string", typ: "webhook", raw: longWebhook, wantErr: true},
		{name: "webhook object", typ: "webhook", raw: json.RawMessage(`{}`), wantErr: true},
		{name: "slack object", typ: "slack", raw: json.RawMessage(`{"webhook_url":"x"}`), want: newSecret{[]byte(`{"webhook_url":"x"}`)}},
		{name: "slack string", typ: "slack", raw: json.RawMessage(`"s"`), wantErr: true},
		{name: "oversized slack object", typ: "slack", raw: largeObject, wantErr: true},
		{name: "webhook null", typ: "webhook", raw: json.RawMessage(`null`), want: newSecret{}},
		{name: "slack null", typ: "slack", raw: json.RawMessage(`null`), want: newSecret{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := destinationSecret(test.typ, test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("destinationSecret() error = %v, want error %v", err, test.wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, test.want) {
				t.Errorf("destinationSecret() = %q, want %q", got, test.want)
			}
		})
	}
}
