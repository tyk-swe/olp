package codeplans

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestManifestsSurviveTheHostRoundTripAndDeclareValidProfiles(t *testing.T) {
	for _, a := range []Adapter{OpenCodeGo(), ZAI()} {
		m := a.Manifest()
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var decoded abi.Manifest
		if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(decoded.Profiles, m.Profiles) || !reflect.DeepEqual(decoded.Origins, m.Origins) {
			t.Fatalf("%s manifest changes across JSON: %v", m.Name, err)
		}
		for _, p := range m.Profiles {
			if _, err := connectors.NewPluginProfile("digest", m, p.ID); err != nil {
				t.Fatalf("%s: %v", p.ID, err)
			}
			if p.Grant == nil || p.Grant.Input != abi.GrantInputSecret || len(p.Grant.Facts) != 0 || !p.Signing {
				t.Fatalf("%s must be a signing-fenced pasted-key grant", p.ID)
			}
			for _, address := range []string{p.Hosting.Address, a.keyPages[p.ID]} {
				u, err := url.Parse(address)
				if err != nil || u.Scheme != "https" || !slices.Contains(m.Origins, connectors.Origin(u)) {
					t.Fatalf("%s: %s is not at an approved origin", p.ID, address)
				}
			}
		}
	}
}

func TestEnrollmentAcceptsOnlyAPastedVendorKey(t *testing.T) {
	a := ZAI()
	ctx := context.Background()
	if _, err := a.StartGrant(ctx, abi.GrantStart{Profile: OpenCodeGoProfile}); err == nil {
		t.Fatal("another plugin's profile started enrollment")
	}
	start, err := a.StartGrant(ctx, abi.GrantStart{Profile: BigModelProfile})
	if err != nil || start.URL != BigModelKeyPage || start.Device != nil {
		t.Fatalf("start: %+v %v", start, err)
	}
	key := "0123456789abcdef0123456789abcdef.AbCdEfGhIjKlMnOp"
	grant, err := a.ExchangeGrant(ctx, abi.GrantExchange{Profile: BigModelProfile, Session: start.Session, Input: "  " + key + "\n"})
	if err != nil || grant.AccessToken != key || grant.Principal != Principal(BigModelProfile, key) || grant.RefreshToken != "" || grant.ExpiresIn != 0 || grant.Facts != nil {
		t.Fatalf("grant: %+v %v", grant, err)
	}
	if strings.Contains(grant.Principal, key) || !strings.HasPrefix(grant.Principal, BigModelProfile+":") || len(grant.Principal) > 256 {
		t.Fatalf("principal reveals or exceeds: %s", grant.Principal)
	}
	if Principal(ZAIProfile, key) == grant.Principal || Principal(BigModelProfile, key+"x") == grant.Principal {
		t.Fatal("principal is not separated by profile and key")
	}
	if _, err := a.ExchangeGrant(ctx, abi.GrantExchange{Profile: ZAIProfile, Session: start.Session, Input: key}); !isCode(err, abi.CodeStateMismatch) {
		t.Fatalf("profile mismatch accepted: %v", err)
	}
	for _, input := range []string{"", "short-key", "olp_abcdefgh_0123456789abcdef", "https://z.ai/manage-apikey/apikey-list", "0123456789abcdef 0123456789abcdef", "0123456789abcdef\r\n0123", strings.Repeat("k", 513), "0123456789abcdéf0123"} {
		if _, err := a.ExchangeGrant(ctx, abi.GrantExchange{Profile: BigModelProfile, Session: start.Session, Input: input}); !isCode(err, abi.CodeInvalidRequest) {
			t.Fatalf("%q accepted: %v", input, err)
		}
	}
	if _, err := a.Sign(ctx, abi.SignRequest{}); !isCode(err, "code_mode_required") {
		t.Fatalf("ordinary signing was not fenced: %v", err)
	}
}

func isCode(err error, code string) bool {
	failure, ok := errors.AsType[*abi.Error](err)
	return ok && failure.Code == code
}
