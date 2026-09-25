package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestConfigurationRejectsInvalidDefaults(t *testing.T) {
	for _, raw := range []string{
		`{"conversation":"conv_other"}`,
		`{"max_tokens":1,"max_completion_tokens":2}`,
		`{"max_output_tokens":"10"}`, `{"stream":true}`, `{"input":"hidden"}`,
	} {
		cfg := Configuration{Kind: KindOpenAICompatible, AuthMode: AuthNone, Endpoint: new("https://example.com/v1")}
		cfg.Normalize()
		if err := json.Unmarshal([]byte(raw), &cfg.Options.ParameterDefaults); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(&egress.Policy{}); err == nil {
			t.Errorf("accepted defaults %s", raw)
		}
	}
}

func TestModelDiscoveryRequiresAModelList(t *testing.T) {
	var listing atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(listing.Load().(string)))
	}))
	defer upstream.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	cfg := Configuration{Kind: KindOpenAICompatible, AuthMode: AuthNone, Endpoint: new(upstream.URL + "/v1")}
	cfg.Normalize()
	discover := func(raw string) ([]string, error) {
		listing.Store(raw)
		return New(nil, policy).listModels(context.Background(), &cfg, nil)
	}
	for _, raw := range []string{`null`, `{}`, `{"error":{"message":"unauthorized"}}`, `{"data":null}`, `{"data":{}}`} {
		if _, err := discover(raw); err == nil {
			t.Errorf("accepted model response %s", raw)
		}
	}
	if models, err := discover(`{"data":[]}`); err != nil || len(models) != 0 {
		t.Fatalf("valid empty list: %v %v", models, err)
	}
	models, err := discover(`{"data":[{"id":"good"},{"id":"bad\u0000model"},{"id":"bad\nmodel"},{"id":"good"}]}`)
	if err != nil || !reflect.DeepEqual(models, []string{"good"}) {
		t.Fatalf("model identifiers: %q %v", models, err)
	}
}

func TestSlotUUIDsAreCanonical(t *testing.T) {
	const id = "abcdef01-2345-4678-9abc-def012345678"
	for _, value := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "urn:uuid:" + id} {
		in := slotInput{ID: value, Name: "slot", CredentialVersionID: &value, AllowedAPIKeys: []string{value}}
		if err := validSlot(&in, id); err != nil {
			t.Fatal(err)
		}
		if in.ID != id || *in.CredentialVersionID != id || in.AllowedAPIKeys[0] != id {
			t.Fatalf("noncanonical UUIDs retained: %+v", in)
		}
	}
}
