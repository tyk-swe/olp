package codeplans

import (
	"context"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestExchangeGrantPrincipalsFollowProfileAndKey(t *testing.T) {
	adapter := ZAI()
	start, err := adapter.StartGrant(context.Background(), abi.GrantStart{Profile: ZAIProfile})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := adapter.ExchangeGrant(context.Background(), abi.GrantExchange{Profile: ZAIProfile, Session: start.Session, Input: "  vendor-key-1234567890  "})
	if err != nil {
		t.Fatal(err)
	}
	if grant.AccessToken != "vendor-key-1234567890" {
		t.Fatalf("pasted key not trimmed: %q", grant.AccessToken)
	}
	if grant.Principal != Principal(ZAIProfile, grant.AccessToken) || strings.Contains(grant.Principal, grant.AccessToken) {
		t.Fatalf("principal does not fingerprint the key: %q", grant.Principal)
	}
	if Principal(BigModelProfile, grant.AccessToken) == grant.Principal {
		t.Fatal("principal ignores the profile account system")
	}
}

func TestExchangeGrantRejectsOLPKeysAndProfileMismatch(t *testing.T) {
	adapter := OpenCodeGo()
	start, err := adapter.StartGrant(context.Background(), abi.GrantStart{Profile: OpenCodeGoProfile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ExchangeGrant(context.Background(), abi.GrantExchange{Profile: OpenCodeGoProfile, Session: start.Session, Input: "olp_12345678901234567890"}); err == nil {
		t.Fatal("OLP key accepted as vendor key")
	}
	if _, err := adapter.ExchangeGrant(context.Background(), abi.GrantExchange{Profile: ZAIProfile, Session: start.Session, Input: "vendor-key-1234567890"}); err == nil {
		t.Fatal("cross-profile enrollment accepted")
	}
}

func TestSignFencesOrdinaryInference(t *testing.T) {
	_, err := ZAI().Sign(context.Background(), abi.SignRequest{})
	if err == nil || !strings.Contains(err.Error(), "code_mode_required") {
		t.Fatalf("sign not fenced: %v", err)
	}
}
