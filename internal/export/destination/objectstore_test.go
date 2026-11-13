package destination

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestObjectStoreSigningRegion(t *testing.T) {
	// AWS endpoint syntax, checked November 12, 2026: https://docs.aws.amazon.com/general/latest/gr/s3.html and https://docs.aws.amazon.com/AmazonS3/latest/userguide/VirtualHosting.html
	for _, tc := range []struct{ name, host, explicit, environment, want string }{
		{"regional", "bucket.s3.us-west-2.amazonaws.com", "", "fallback-region", "us-west-2"},
		{"dualstack", "bucket.s3.dualstack.us-west-2.amazonaws.com", "", "fallback-region", "us-west-2"},
		{"global", "bucket.s3.amazonaws.com", "", "fallback-region", "fallback-region"},
		{"override", "bucket.s3.dualstack.us-west-2.amazonaws.com", "explicit-region", "fallback-region", "explicit-region"},
		{"fips dualstack", "bucket.s3-fips.dualstack.us-west-2.amazonaws.com", "", "fallback-region", "us-west-2"},
		{"path style", "s3.us-west-2.amazonaws.com", "", "fallback-region", "us-west-2"},
		{"legacy", "bucket.s3-us-west-2.amazonaws.com", "", "fallback-region", "us-west-2"},
		{"custom", "bucket.s3.example.test", "", "fallback-region", "fallback-region"},
		{"accelerate", "bucket.s3-accelerate.amazonaws.com", "", "fallback-region", "fallback-region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := LoadStore(FOCUS{Provider: "s3"}, "https://"+tc.host, []byte(`{"auth_mode":"static","region":"`+tc.explicit+`","secret":{"access_key_id":"fixture-access-key","secret_access_key":"fixture-secret-key"}}`))
			if err != nil {
				t.Fatal(err)
			}
			sender := NewSender(&egress.Policy{}, func(name string) string {
				if name == "AWS_REGION" {
					return tc.environment
				}
				return ""
			}, "installation")
			endpoint, _ := url.Parse("https://" + tc.host)
			req := httptest.NewRequest("PUT", endpoint.String()+"/part.jsonl", strings.NewReader("fixture"))
			if err := sender.authorize(t.Context(), req, store, endpoint, []byte("fixture")); err != nil {
				t.Fatal(err)
			}
			auth := req.Header.Get("Authorization")
			_, scope, found := strings.Cut(auth, "Credential=")
			fields := strings.Split(strings.Split(scope, ",")[0], "/")
			if !found || len(fields) < 5 {
				t.Fatal("signature has no credential scope")
			}
			if fields[2] != tc.want {
				t.Errorf("signed region %q, want %q", fields[2], tc.want)
			}
		})
	}
}

func TestSinkValidation(t *testing.T) {
	for _, raw := range []string{`{"typo":null}`, `{"typo":1}`} {
		if _, err := ParseSpec("https", "json", json.RawMessage(raw)); err == nil {
			t.Errorf("accepted unknown setting %s", raw)
		}
	}
	if _, err := ParseSpec("https", "cloudevents", json.RawMessage(`{"source":null}`)); err != nil {
		t.Fatalf("known null setting: %v", err)
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"outcome":"<nil>"}`, false}, {`{"outcome":"bogus"}`, false},
		{`{"outcome":"success"}`, true}, {`{"outcome":"failure"}`, true}, {`{"outcome":null}`, true}, {`{}`, true},
	} {
		_, err := (&API{}).filter(t.Context(), nil, json.RawMessage(tc.raw), nil)
		if (err == nil) != tc.valid {
			t.Errorf("filter %s: %v", tc.raw, err)
		}
	}
}
