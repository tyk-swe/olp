package access

import "testing"

func TestKeyNetworkPolicyValidationAndMatching(t *testing.T) {
	for _, cidrs := range [][]string{nil, {}, {"192.0.2.1/24", "2001:db8::/32"}, {"0.0.0.0/0", "::/0"}} {
		if err := validateKeyCIDRs(cidrs); err != nil {
			t.Fatal(err)
		}
	}
	for _, cidrs := range [][]string{{"192.0.2.1"}, {"example.com/24"}, {"192.0.2.1/33"}, {"2001:db8::/129"}, {"::ffff:192.0.2.0/120"}, {"192.0.2.0/24", "192.0.2.1/24"}, make([]string, 65)} {
		if err := validateKeyCIDRs(cidrs); err == nil {
			t.Fatalf("accepted invalid CIDRs %v", cidrs)
		}
	}
	authority := Authority{Policy: KeyPolicy{AllowedCIDRs: []string{"192.0.2.1/24", "2001:db8::/32"}}}
	for _, addr := range []string{"192.0.2.0", "192.0.2.255", "::ffff:192.0.2.12", "2001:db8::1234"} {
		if !authority.AllowsClientIP(addr) {
			t.Fatalf("refused %s", addr)
		}
	}
	for _, addr := range []string{"192.0.3.1", "2001:db9::1", "", "not-an-address", "2001:db8::1%zone"} {
		if authority.AllowsClientIP(addr) {
			t.Fatalf("allowed %s", addr)
		}
	}
	authority.Policy.AllowedCIDRs = append(authority.Policy.AllowedCIDRs, "invalid")
	if authority.AllowsClientIP("192.0.2.1") {
		t.Fatal("invalid stored policy did not fail closed")
	}
	if !(Authority{}).AllowsClientIP("") {
		t.Fatal("unrestricted key unexpectedly required an address")
	}
}
