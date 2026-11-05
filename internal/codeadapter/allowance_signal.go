package codeadapter

import "github.com/tyk-swe/olp/internal/codemode"

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

var allowanceSignals = map[codemode.Adapter]AllowanceSignalInfo{
	codemode.AdapterCodex:       {Signal: AllowanceSignalProven},
	codemode.AdapterMiniMaxPlan: {Signal: AllowanceSignalRemainsUnqualified, Reason: "schema-unpinned"},
}

func (v Vendor) AllowanceSignal() AllowanceSignalInfo {
	if info, ok := allowanceSignals[v.Adapter]; ok {
		return info
	}
	return AllowanceSignalInfo{Signal: AllowanceSignalNone}
}

func AllowanceSignalForProfile(kind, authMode, profileID string) (AllowanceSignalInfo, bool) {
	vendor, ok := ForConnection(kind, authMode, profileID)
	if !ok {
		return AllowanceSignalInfo{}, false
	}
	return vendor.AllowanceSignal(), true
}
