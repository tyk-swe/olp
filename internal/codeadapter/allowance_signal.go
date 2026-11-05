package codeadapter

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
)

type AllowanceSignal string

const (
	AllowanceSignalProven             AllowanceSignal = "proven"
	AllowanceSignalNone               AllowanceSignal = "none"
	AllowanceSignalRemainsUnqualified AllowanceSignal = "remains-unqualified"
)

type AllowanceSignalInfo struct {
	Signal AllowanceSignal
	Reason string
}

func (info AllowanceSignalInfo) QualifiedForActiveRead() bool {
	return info.Signal == AllowanceSignalProven
}

var allowanceSignals = map[string]AllowanceSignalInfo{
	codexauth.ProfileID:        {Signal: AllowanceSignalProven},
	codeplans.MiniMaxProfile:   {Signal: AllowanceSignalRemainsUnqualified, Reason: "schema-unpinned"},
	codeplans.MiniMaxCNProfile: {Signal: AllowanceSignalRemainsUnqualified, Reason: "schema-unpinned"},
}

func (v Vendor) AllowanceSignal(profileID string) AllowanceSignalInfo {
	if !slices.Contains(v.profiles, profileID) {
		return AllowanceSignalInfo{Signal: AllowanceSignalNone}
	}
	if info, ok := allowanceSignals[profileID]; ok {
		return info
	}
	return AllowanceSignalInfo{Signal: AllowanceSignalNone}
}

func QualifiedProfileIDs() []string {
	qualified := make([]string, 0, 1)
	for _, vendor := range vendors {
		for _, profile := range vendor.profiles {
			if vendor.AllowanceSignal(profile).QualifiedForActiveRead() {
				qualified = append(qualified, profile)
			}
		}
	}
	return qualified
}

func SQLAllowanceSignal(config string) string {
	var b strings.Builder
	b.WriteString("CASE WHEN " + config + "->>'kind'='" + kindPlugin + "' AND " + config + "->>'auth_mode'='" + authGrant + "' THEN CASE " + config + "->>'profile_id'")
	for _, vendor := range vendors {
		for _, profile := range vendor.profiles {
			if info, ok := allowanceSignals[profile]; ok {
				b.WriteString(" WHEN '" + profile + "' THEN '" + string(info.Signal) + "'")
			}
		}
	}
	b.WriteString(" ELSE '" + string(AllowanceSignalNone) + "' END ELSE '" + string(AllowanceSignalNone) + "' END")
	return b.String()
}

func SQLAllowanceSignalReason(config string) string {
	var b strings.Builder
	b.WriteString("CASE WHEN " + config + "->>'kind'='" + kindPlugin + "' AND " + config + "->>'auth_mode'='" + authGrant + "' THEN CASE " + config + "->>'profile_id'")
	for _, vendor := range vendors {
		for _, profile := range vendor.profiles {
			if info, ok := allowanceSignals[profile]; ok && info.Reason != "" {
				b.WriteString(" WHEN '" + profile + "' THEN '" + info.Reason + "'")
			}
		}
	}
	b.WriteString(" END END")
	return b.String()
}

func AllowanceSignalForProfile(kind, authMode, profileID string) (AllowanceSignalInfo, bool) {
	vendor, ok := ForConnection(kind, authMode, profileID)
	if !ok {
		return AllowanceSignalInfo{}, false
	}
	return vendor.AllowanceSignal(profileID), true
}
