package grants

import (
	"strings"
	"testing"
	"time"

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

// A device authorization a plugin starts must send the operator to one of the
// plugin's approved origins with a user code OLP can show, and OLP polls it
// within its bounds: every 5 seconds unless it says otherwise, for as long as
// its user code lasts, and at most 30 minutes.
func TestDeviceAuthorizationsMustFitOLPsBounds(t *testing.T) {
	manifest := abi.Manifest{Origins: []string{"https://login.acme.example"}}
	valid := func() abi.DeviceAuthorization {
		return abi.DeviceAuthorization{VerificationURL: "https://login.acme.example/device", UserCode: "WDJB-MJHT", ExpiresIn: 900}
	}
	var e Enrollment
	if err := e.authorizeDevice(manifest, valid()); err != nil {
		t.Fatal(err)
	}
	if e.Device.Interval != 5 || e.interval != defaultInterval || time.Until(e.ExpiresAt) > 15*time.Minute || time.Until(e.ExpiresAt) < 14*time.Minute {
		t.Fatalf("enrollment %+v, device %+v", e, e.Device)
	}
	lasting := valid()
	lasting.ExpiresIn, lasting.Interval = 1<<62, 10
	if err := e.authorizeDevice(manifest, lasting); err != nil || e.interval != 10*time.Second || time.Until(e.ExpiresAt) > maxDeviceTTL {
		t.Fatalf("a long-lived device authorization: %v, expires %v", err, e.ExpiresAt)
	}
	for name, mutate := range map[string]func(*abi.DeviceAuthorization){
		"unapproved origin":    func(d *abi.DeviceAuthorization) { d.VerificationURL = "https://phish.example/device" },
		"not a URL":            func(d *abi.DeviceAuthorization) { d.VerificationURL = "login.acme.example/device" },
		"no user code":         func(d *abi.DeviceAuthorization) { d.UserCode = "" },
		"long user code":       func(d *abi.DeviceAuthorization) { d.UserCode = strings.Repeat("W", maxUserCode+1) },
		"control in user code": func(d *abi.DeviceAuthorization) { d.UserCode = "WDJB\nMJHT" },
		"no lifetime":          func(d *abi.DeviceAuthorization) { d.ExpiresIn = 0 },
		"negative interval":    func(d *abi.DeviceAuthorization) { d.Interval = -1 },
		"long interval":        func(d *abi.DeviceAuthorization) { d.Interval = 301 },
	} {
		t.Run(name, func(t *testing.T) {
			device := valid()
			mutate(&device)
			if (&Enrollment{}).authorizeDevice(manifest, device) == nil {
				t.Fatal("accepted")
			}
		})
	}
}
