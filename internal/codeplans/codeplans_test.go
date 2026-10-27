package codeplans

import (
	"context"
	"slices"
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

func TestStartGrantSendsOperatorsToEachPlanKeyPage(t *testing.T) {
	for _, test := range []struct {
		adapter Adapter
		profile string
		page    string
	}{
		{MiniMax(), MiniMaxProfile, MiniMaxKeyPage},
		{MiniMax(), MiniMaxCNProfile, MiniMaxCNKeyPage},
		{Kimi(), KimiProfile, KimiKeyPage},
		{Kimi(), KimiCNProfile, KimiKeyPage},
		{QwenTokenPlan(), QwenTokenPlanProfile, QwenTokenPlanKeyPage},
		{QwenTokenPlan(), QwenTokenPlanCNProfile, QwenTokenPlanCNKeyPage},
	} {
		if !slices.ContainsFunc(test.adapter.Manifest().Profiles, func(profile abi.Profile) bool { return profile.ID == test.profile }) {
			t.Fatalf("plugin does not advertise enrollment profile %s", test.profile)
		}
		start, err := test.adapter.StartGrant(context.Background(), abi.GrantStart{Profile: test.profile})
		if err != nil || start.URL != test.page {
			t.Fatalf("%s opened %q: %v", test.profile, start.URL, err)
		}
	}
}

// MiniMax issues sk-cp- plan keys and Alibaba sk-sp- ones, beside pay-as-you-go
// keys that share the host and bill outside the plan:
// https://platform.minimax.io/docs/m-plan/other-tools and
// https://www.alibabacloud.com/help/en/model-studio/token-plan-team-quickstart,
// both checked 2026-10-26.
func TestExchangeGrantRefusesPayAsYouGoKeys(t *testing.T) {
	for _, test := range []struct {
		adapter    Adapter
		profile    string
		payg, plan string
	}{
		{MiniMax(), MiniMaxProfile, "sk-api-0123456789abcdef", "sk-cp-0123456789abcdef"},
		{MiniMax(), MiniMaxCNProfile, "sk-api-0123456789abcdef", "sk-cp-0123456789abcdef"},
		{QwenTokenPlan(), QwenTokenPlanProfile, "sk-0123456789abcdef0", "sk-sp-0123456789abcdef"},
		{QwenTokenPlan(), QwenTokenPlanCNProfile, "sk-0123456789abcdef0", "sk-sp-0123456789abcdef"},
	} {
		start, err := test.adapter.StartGrant(context.Background(), abi.GrantStart{Profile: test.profile})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := test.adapter.ExchangeGrant(context.Background(), abi.GrantExchange{Profile: test.profile, Session: start.Session, Input: test.payg}); err == nil {
			t.Fatalf("%s accepted a pay-as-you-go key", test.profile)
		}
		grant, err := test.adapter.ExchangeGrant(context.Background(), abi.GrantExchange{Profile: test.profile, Session: start.Session, Input: test.plan})
		if err != nil || grant.AccessToken != test.plan || grant.Principal != Principal(test.profile, test.plan) {
			t.Fatalf("%s refused the plan key: %v %v", test.profile, grant, err)
		}
	}
}

func TestExchangeGrantAcceptsKimiKey(t *testing.T) {
	adapter := Kimi()
	start, err := adapter.StartGrant(context.Background(), abi.GrantStart{Profile: KimiProfile})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := adapter.ExchangeGrant(context.Background(), abi.GrantExchange{Profile: KimiProfile, Session: start.Session, Input: "kimi-key-0123456789abcdef"})
	if err != nil || grant.AccessToken != "kimi-key-0123456789abcdef" {
		t.Fatalf("Kimi Code key refused: %v", err)
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
