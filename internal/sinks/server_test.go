package sinks

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestSinkValidationBoundsScopeAndDoesNotWeakenEgress(t *testing.T) {
	enabled := true
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	base := input{Name: " sink ", Destination: "http://127.0.0.1/export", Streams: []string{"requests", "attempts"}, Enabled: &enabled}
	if err := validate(&base, policy, nil); err != nil || base.Name != "sink" || !slices.Equal(base.Streams, []string{"requests", "attempts"}) {
		t.Fatalf("valid sink: %v", err)
	}
	for _, test := range []struct {
		streams     []string
		destination string
		project     *string
	}{
		{[]string{"requests", "requests"}, base.Destination, nil},
		{[]string{"content"}, base.Destination, nil},
		{[]string{"audit"}, base.Destination, new("project")},
		{[]string{"requests"}, "http://169.254.169.254/metadata", nil},
	} {
		in := base
		in.Streams = test.streams
		in.Destination = test.destination
		if validate(&in, policy, test.project) == nil {
			t.Fatalf("accepted unsafe sink: %v", test)
		}
	}
	if validate(&base, &egress.Policy{}, nil) == nil {
		t.Fatal("private HTTP destination bypassed default egress")
	}
}
