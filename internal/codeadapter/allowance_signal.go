package codeadapter

import (
	"slices"

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

func AllowanceSignalForProfile(kind, authMode, profileID string) (AllowanceSignalInfo, bool) {
	vendor, ok := ForConnection(kind, authMode, profileID)
	if !ok {
		return AllowanceSignalInfo{}, false
	}
	return vendor.AllowanceSignal(profileID), true
}
