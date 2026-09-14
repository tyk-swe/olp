package egress

import (
	"net/http"
	"reflect"
	"testing"
)

func TestCredentialHeadersDecodeValuesAndMatchNames(t *testing.T) {
	for _, tc := range []struct {
		name, credential string
		valid            bool
	}{
		{"distinct values", `{"X-Api-Key":"secret","X-Tenant":"tenant"}`, true},
		{"case insensitive", `{"x-api-key":"secret","X-TENANT":"tenant"}`, true},
		{"plain secret", `secret`, false},
		{"null", `null`, false},
		{"empty", `{}`, false},
		{"missing name", `{"X-Api-Key":"secret"}`, false},
		{"unexpected name", `{"X-Api-Key":"secret","X-Other":"tenant"}`, false},
		{"extra name", `{"X-Api-Key":"secret","X-Tenant":"tenant","X-Extra":"secret"}`, false},
		{"null value", `{"X-Api-Key":null,"X-Tenant":"tenant"}`, false},
		{"number value", `{"X-Api-Key":42,"X-Tenant":"tenant"}`, false},
		{"duplicate canonical name", `{"X-Api-Key":"secret","x-api-key":"tenant"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{"Accept": {"application/json"}}
			before := header.Clone()
			err := ApplyCredentialHeaders(header, []string{"X-Api-Key", "X-Tenant"}, []byte(tc.credential))
			if !tc.valid {
				if err == nil || !reflect.DeepEqual(header, before) {
					t.Fatalf("invalid credential changed headers: %v, %v", header, err)
				}
				return
			}
			if err != nil || header.Get("X-Api-Key") != "secret" || header.Get("X-Tenant") != "tenant" || header.Get("Accept") != "application/json" {
				t.Fatalf("headers %v: %v", header, err)
			}
		})
	}
}

func TestDuplicateConfiguredHeadersCannotAllowUnexpectedSecretHeaders(t *testing.T) {
	header := http.Header{}
	err := ApplyCredentialHeaders(header, []string{"X-Api-Key", "x-api-key"}, []byte(`{"X-Api-Key":"secret","Authorization":"unexpected"}`))
	if err == nil || len(header) != 0 {
		t.Fatalf("unexpected credential header applied: %v, %v", header, err)
	}
}
