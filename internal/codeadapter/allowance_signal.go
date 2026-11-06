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
	Signal          AllowanceSignal
	Reason          string
	ActiveQualified bool
}

var allowanceSignals = map[string]AllowanceSignalInfo{
	codexauth.ProfileID:        {Signal: AllowanceSignalProven},
	codeplans.MiniMaxProfile:   {Signal: AllowanceSignalRemainsUnqualified, Reason: "live-qualification-pending"},
	codeplans.MiniMaxCNProfile: {Signal: AllowanceSignalRemainsUnqualified, Reason: "live-qualification-pending"},
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
	return slices.Collect(func(yield func(string) bool) {
		for _, vendor := range vendors {
			for _, profile := range vendor.profiles {
				if vendor.AllowanceSignal(profile).ActiveQualified && !yield(profile) {
					return
				}
			}
		}
	})
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
	clauses := []string{}
	for _, vendor := range vendors {
		for _, profile := range vendor.profiles {
			if info, ok := allowanceSignals[profile]; ok && info.Reason != "" {
				clauses = append(clauses, "WHEN '"+profile+"' THEN '"+info.Reason+"'")
			}
		}
	}
	if len(clauses) == 0 {
		return "NULL::text"
	}
	return "CASE WHEN " + config + "->>'kind'='" + kindPlugin + "' AND " + config + "->>'auth_mode'='" + authGrant + "' THEN CASE " + config + "->>'profile_id' " + strings.Join(clauses, " ") + " END END"
}

func AllowanceSignalForProfile(kind, authMode, profileID string) (AllowanceSignalInfo, bool) {
	vendor, ok := ForConnection(kind, authMode, profileID)
	if !ok {
		return AllowanceSignalInfo{}, false
	}
	return vendor.AllowanceSignal(profileID), true
}
