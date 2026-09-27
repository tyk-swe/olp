package grants

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A grant a plugin reports must be one the profile's hosting adaptation can
// place: an access token that fits a header, an observed principal, and
// exactly the grant facts the profile declares.
func TestGrantsMustFitTheirProfile(t *testing.T) {
	declared := &abi.GrantAuthentication{Facts: []string{"account"}}
	valid := func() abi.Grant {
		return abi.Grant{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600, Principal: "user@acme.example", Facts: map[string]string{"account": "7"}}
	}
	grant := valid()
	if err := validate(declared, &grant); err != nil {
		t.Fatal(err)
	}
	none := abi.Grant{AccessToken: "at", Principal: "user@acme.example"}
	if err := validate(&abi.GrantAuthentication{}, &none); err != nil || none.Facts == nil {
		t.Fatalf("a grant without facts: %v %v", err, none.Facts)
	}
	for name, mutate := range map[string]func(*abi.Grant){
		"no access token":        func(g *abi.Grant) { g.AccessToken = "" },
		"access token line":      func(g *abi.Grant) { g.AccessToken = "at\r\nX-Injected: 1" },
		"long refresh token":     func(g *abi.Grant) { g.RefreshToken = strings.Repeat("r", maxToken+1) },
		"negative expiry":        func(g *abi.Grant) { g.ExpiresIn = -1 },
		"no principal":           func(g *abi.Grant) { g.Principal = "" },
		"control in principal":   func(g *abi.Grant) { g.Principal = "user\n" },
		"undeclared fact":        func(g *abi.Grant) { g.Facts["project"] = "p" },
		"missing declared fact":  func(g *abi.Grant) { delete(g.Facts, "account") },
		"control in fact":        func(g *abi.Grant) { g.Facts["account"] = "7\x00" },
		"fact beyond its bounds": func(g *abi.Grant) { g.Facts["account"] = strings.Repeat("a", maxFact+1) },
	} {
		t.Run(name, func(t *testing.T) {
			grant := valid()
			mutate(&grant)
			if validate(declared, &grant) == nil {
				t.Fatal("accepted")
			}
		})
	}
}
