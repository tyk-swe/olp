package codeallowance

import (
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

const UsageURL = "https://chatgpt.com/backend-api/wham/usage"

const maxUsageBytes = 256 << 10

func ParseUsage(body []byte, now time.Time) (codemode.Allowance, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxUsageBytes})
	if err != nil || now.IsZero() {
		return codemode.Allowance{}, false
	}
	root := document.Root()
	plan, present, valid := member(root, "plan_type")
	if !valid || !present {
		return codemode.Allowance{}, false
	}
	if text, ok := plan.Text(); !ok || text == "" {
		return codemode.Allowance{}, false
	}
	allowance := codemode.Allowance{ObservedAt: now}
	windows, ok := rateLimitWindows(root, "codex", now)
	if !ok {
		return codemode.Allowance{}, false
	}
	allowance.Windows = append(allowance.Windows, windows...)
	credits, ok := usageCredits(root, now)
	if !ok {
		return codemode.Allowance{}, false
	}
	allowance.Credits = credits
	additional, present, valid := member(root, "additional_rate_limits")
	if !valid {
		return codemode.Allowance{}, false
	}
	if present && additional.Kind() != oif.Null {
		if additional.Kind() != oif.Array {
			return codemode.Allowance{}, false
		}
		for _, entry := range additional.Elements() {
			entryWindows, usable, entryOK := additionalWindows(entry, now)
			if !entryOK {
				return codemode.Allowance{}, false
			}
			if usable {
				allowance.Windows = append(allowance.Windows, entryWindows...)
			}
		}
	}
	allowance.Normalize()
	if len(allowance.Windows) == 0 && allowance.Credits == nil || allowance.Validate() != nil {
		return codemode.Allowance{}, false
	}
	return allowance, true
}

func member(object oif.Value, name string) (oif.Value, bool, bool) {
	if object.Kind() != oif.Object {
		return oif.Value{}, false, false
	}
	found := oif.Value{}
	present := false
	for _, entry := range object.Members() {
		if entry.Name != name {
			continue
		}
		if present {
			return oif.Value{}, false, false
		}
		found, present = entry.Value, true
	}
	return found, present, true
}

func rateLimitWindows(parent oif.Value, limit string, now time.Time) ([]codemode.AllowanceWindow, bool) {
	value, present, valid := member(parent, "rate_limit")
	if !valid {
		return nil, false
	}
	if !present || value.Kind() == oif.Null {
		return nil, true
	}
	if value.Kind() != oif.Object {
		return nil, false
	}
	for _, name := range []string{"allowed", "limit_reached"} {
		flag, flagPresent, flagValid := member(value, name)
		if !flagValid || !flagPresent || flag.Kind() != oif.Boolean {
			return nil, false
		}
	}
	windows := []codemode.AllowanceWindow{}
	for _, name := range []string{"primary", "secondary"} {
		snapshot, snapshotPresent, snapshotValid := member(value, name+"_window")
		if !snapshotValid {
			return nil, false
		}
		if !snapshotPresent || snapshot.Kind() == oif.Null {
			continue
		}
		window, ok := usageWindow(snapshot, limit, name, now)
		if !ok {
			return nil, false
		}
		windows = append(windows, window)
	}
	return windows, true
}

func additionalWindows(entry oif.Value, now time.Time) ([]codemode.AllowanceWindow, bool, bool) {
	if entry.Kind() != oif.Object {
		return nil, false, false
	}
	name, present, valid := member(entry, "limit_name")
	if !valid {
		return nil, false, false
	}
	text, isText := name.Text()
	limit, validLimit := codemode.NormalizeLimitID(text)
	if !present || !isText || !validLimit {
		return nil, false, true
	}
	if feature, featurePresent, featureValid := member(entry, "metered_feature"); !featureValid || featurePresent && feature.Kind() != oif.String {
		return nil, false, false
	}
	windows, ok := rateLimitWindows(entry, limit, now)
	return windows, true, ok
}

func usageWindow(snapshot oif.Value, limit, name string, now time.Time) (codemode.AllowanceWindow, bool) {
	if snapshot.Kind() != oif.Object {
		return codemode.AllowanceWindow{}, false
	}
	used, ok := requiredInt(snapshot, "used_percent")
	if !ok || used < 0 {
		return codemode.AllowanceWindow{}, false
	}
	seconds, ok := requiredInt(snapshot, "limit_window_seconds")
	if !ok || seconds < 0 {
		return codemode.AllowanceWindow{}, false
	}
	resetAt, ok := requiredInt(snapshot, "reset_at")
	if !ok || resetAt <= 0 || resetAt >= 253402300800 {
		return codemode.AllowanceWindow{}, false
	}
	if after, present, valid := optionalInt(snapshot, "reset_after_seconds"); !valid || present && after < 0 {
		return codemode.AllowanceWindow{}, false
	}
	reset := time.Unix(resetAt, 0).UTC()
	window := codemode.AllowanceWindow{
		LimitID: limit, Window: name, Unit: codemode.AllowanceUnitPercent,
		Evidence: codemode.AllowanceEvidenceActive, UsedPercent: float64(used),
		RemainingPercent: max(0, 100-float64(used)), ResetsAt: &reset, ObservedAt: now,
	}
	if seconds > 0 && seconds%60 == 0 {
		minutes := seconds / 60
		window.WindowMinutes = &minutes
	}
	if window.Validate() != nil {
		return codemode.AllowanceWindow{}, false
	}
	return window, true
}

func requiredInt(object oif.Value, name string) (int64, bool) {
	value, present, valid := optionalInt(object, name)
	return value, valid && present
}

func optionalInt(object oif.Value, name string) (int64, bool, bool) {
	return parseIntMember(object, name, 32)
}

func requiredInt64(object oif.Value, name string) (int64, bool) {
	value, present, valid := optionalInt64(object, name)
	return value, valid && present
}

func optionalInt64(object oif.Value, name string) (int64, bool, bool) {
	return parseIntMember(object, name, 64)
}

func parseIntMember(object oif.Value, name string, bitSize int) (int64, bool, bool) {
	value, present, valid := member(object, name)
	if !valid || !present {
		return 0, present, valid
	}
	if value.Kind() != oif.Number {
		return 0, true, false
	}
	parsed, err := strconv.ParseInt(value.Raw(), 10, bitSize)
	return parsed, true, err == nil
}

func usageCredits(root oif.Value, now time.Time) (*codemode.Credits, bool) {
	value, present, valid := member(root, "credits")
	if !valid {
		return nil, false
	}
	if !present || value.Kind() == oif.Null {
		return nil, true
	}
	if value.Kind() != oif.Object {
		return nil, false
	}
	flags := map[string]bool{}
	for _, name := range []string{"has_credits", "unlimited"} {
		flag, flagPresent, flagValid := member(value, name)
		if !flagValid || !flagPresent || flag.Kind() != oif.Boolean {
			return nil, false
		}
		flags[name] = flag.Raw() == "true"
	}
	credits := &codemode.Credits{HasCredits: flags["has_credits"], Unlimited: flags["unlimited"], ObservedAt: now}
	balance, balancePresent, balanceValid := member(value, "balance")
	if !balanceValid {
		return nil, false
	}
	if balancePresent && balance.Kind() != oif.Null {
		text, ok := balance.Text()
		if !ok {
			return nil, false
		}
		credits.Balance = &text
	}
	if credits.Validate() != nil {
		return nil, false
	}
	return credits, true
}
