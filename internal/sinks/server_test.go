package sinks

import (
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
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

func TestValidateDefinitionWorkflowEvidence(t *testing.T) {
	policy := &egress.Policy{}
	project := "project"
	for _, tc := range []struct {
		name    string
		project *string
		streams []string
	}{
		{"global runs", nil, []string{"runs"}},
		{"global spans", nil, []string{"trace_spans"}},
		{"project runs", &project, []string{"runs"}},
		{"project spans", &project, []string{"trace_spans"}},
		{"global mixed", nil, []string{"runs", "trace_spans", "requests", "audit"}},
		{"project mixed", &project, []string{"runs", "trace_spans", "requests", "attempts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := Definition{ProjectID: tc.project, Name: " evidence ", Type: "https", Destination: "https://EXAMPLE.test/export/", Streams: tc.streams}
			if err := ValidateDefinition(&definition, policy); err != nil {
				t.Fatalf("valid evidence sink rejected: %v", err)
			}
			if definition.Name != "evidence" || definition.Destination != "https://example.test/export" || !slices.Equal(definition.Streams, tc.streams) {
				t.Fatalf("unexpected validated definition: %+v", definition)
			}
		})
	}

	for _, tc := range []struct {
		name        string
		typ         string
		destination string
		project     *string
		streams     []string
		policy      *egress.Policy
		field       string
		status      int
	}{
		{"duplicate runs", "https", "https://example.test/export", nil, []string{"runs", "runs"}, policy, "streams", 422},
		{"duplicate spans", "https", "https://example.test/export", &project, []string{"trace_spans", "trace_spans"}, policy, "streams", 422},
		{"project audit", "https", "https://example.test/export", &project, []string{"runs", "audit"}, policy, "streams", 422},
		{"unsupported stream", "https", "https://example.test/export", nil, []string{"trace_spans", "content"}, policy, "streams", 422},
		{"too many streams", "https", "https://example.test/export", nil, []string{"runs", "trace_spans", "requests", "attempts", "audit"}, policy, "streams", 422},
		{"private destination", "https", "https://127.0.0.1/export", nil, []string{"runs"}, policy, "destination", 422},
		{"missing egress policy", "https", "https://example.test/export", nil, []string{"trace_spans"}, nil, "", 503},
		{"unsupported type", "s3", "https://example.test/export", nil, []string{"runs"}, policy, "type", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := Definition{ProjectID: tc.project, Name: "evidence", Type: tc.typ, Destination: tc.destination, Streams: tc.streams}
			err := ValidateDefinition(&definition, tc.policy)
			var problem *access.Problem
			if !errors.As(err, &problem) || problem.Status != tc.status || problem.Field != tc.field {
				t.Fatalf("expected status %d for %q, got %v", tc.status, tc.field, err)
			}
		})
	}
}
