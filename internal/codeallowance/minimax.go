package codeallowance

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	RemainsURL   = "https://api.minimax.io/v1/token_plan/remains"
	RemainsURLCN = "https://api.minimax.cn/v1/token_plan/remains"
)

const maxEpochMillis = int64(253402300800) * 1000

func ParseRemains(body []byte, accountModels []string, now time.Time) (codemode.Allowance, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxUsageBytes})
	if err != nil || now.IsZero() {
		return codemode.Allowance{}, false
	}
	root := document.Root()
	if root.Kind() != oif.Object {
		return codemode.Allowance{}, false
	}
	if !remainsBaseOK(root) {
		return codemode.Allowance{}, false
	}
	remains, present, valid := member(root, "model_remains")
	if !valid || !present || remains.Kind() != oif.Array {
		return codemode.Allowance{}, false
	}
	bucket, ok := selectCodingBucket(remains.Elements(), accountModels)
	if !ok {
		return codemode.Allowance{}, false
	}
	windows, ok := remainsWindows(bucket, now)
	if !ok || len(windows) == 0 {
		return codemode.Allowance{}, false
	}
	allowance := codemode.Allowance{ObservedAt: now, Windows: windows}
	allowance.Normalize()
	if allowance.Validate() != nil {
		return codemode.Allowance{}, false
	}
	return allowance, true
}

func remainsBaseOK(root oif.Value) bool {
	base, present, valid := member(root, "base_resp")
	if !valid {
		return false
	}
	if !present {
		return true
	}
	if base.Kind() != oif.Object {
		return false
	}
	code, ok := requiredInt64(base, "status_code")
	if !ok || code != 0 {
		return false
	}
	message, messagePresent, messageValid := member(base, "status_msg")
	return messageValid && (!messagePresent || message.Kind() == oif.String)
}

func selectCodingBucket(elements []oif.Value, accountModels []string) (oif.Value, bool) {
	candidates := []oif.Value{}
	names := []string{}
	for _, element := range elements {
		if element.Kind() != oif.Object {
			return oif.Value{}, false
		}
		nameValue, present, valid := member(element, "model_name")
		if !valid || !present {
			return oif.Value{}, false
		}
		name, isText := nameValue.Text()
		if !isText {
			return oif.Value{}, false
		}
		if name == "general" || strings.HasPrefix(name, "MiniMax-M") {
			candidates = append(candidates, element)
			names = append(names, name)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	if len(candidates) == 0 {
		return oif.Value{}, false
	}
	exact := []oif.Value{}
	for index, candidate := range candidates {
		if slices.Contains(accountModels, names[index]) {
			exact = append(exact, candidate)
		}
	}
	if len(exact) == 1 {
		return exact[0], true
	}
	return oif.Value{}, false
}

type remainsWindowInput struct {
	name         string
	start, end   int64
	percent      float64
	percentFound bool
	status       int64
	statusFound  bool
}

func remainsWindows(bucket oif.Value, now time.Time) ([]codemode.AllowanceWindow, bool) {
	required := map[string]int64{}
	for _, name := range []string{
		"start_time", "end_time", "remains_time",
		"current_interval_total_count", "current_interval_usage_count",
		"current_weekly_total_count", "current_weekly_usage_count",
		"weekly_start_time", "weekly_end_time", "weekly_remains_time",
	} {
		value, ok := requiredInt64(bucket, name)
		if !ok || value < 0 {
			return nil, false
		}
		required[name] = value
	}
	if boost, present, valid := optionalInt64(bucket, "weekly_boost_permille"); !valid || present && boost < 0 {
		return nil, false
	}
	interval, ok := remainsWindowInputFor(bucket, "primary", required["start_time"], required["end_time"], "current_interval_remaining_percent", "current_interval_status")
	if !ok {
		return nil, false
	}
	weekly, ok := remainsWindowInputFor(bucket, "weekly", required["weekly_start_time"], required["weekly_end_time"], "current_weekly_remaining_percent", "current_weekly_status")
	if !ok {
		return nil, false
	}
	if required["current_interval_total_count"] == 0 && required["current_weekly_total_count"] == 0 &&
		interval.statusFound && interval.status == 3 && weekly.statusFound && weekly.status == 3 {
		return nil, true
	}
	windows := []codemode.AllowanceWindow{}
	for _, input := range []remainsWindowInput{interval, weekly} {
		window, emit, validWindow := remainsWindow(input, now)
		if !validWindow {
			return nil, false
		}
		if emit {
			windows = append(windows, window)
		}
	}
	return windows, true
}

func remainsWindowInputFor(bucket oif.Value, name string, start, end int64, percentName, statusName string) (remainsWindowInput, bool) {
	input := remainsWindowInput{name: name, start: start, end: end}
	percent, percentPresent, percentValid := optionalPercent(bucket, percentName)
	if !percentValid {
		return remainsWindowInput{}, false
	}
	input.percent, input.percentFound = percent, percentPresent
	status, statusPresent, statusValid := optionalInt64(bucket, statusName)
	if !statusValid || statusPresent && (status < 1 || status > 3) {
		return remainsWindowInput{}, false
	}
	input.status, input.statusFound = status, statusPresent
	return input, true
}

func remainsWindow(input remainsWindowInput, now time.Time) (codemode.AllowanceWindow, bool, bool) {
	if input.statusFound && input.status == 3 {
		return codemode.AllowanceWindow{}, false, true
	}
	remaining, found := func() (float64, bool) {
		if input.statusFound && input.status == 2 {
			return 0, true
		}
		return input.percent, input.percentFound
	}()
	if !found {
		return codemode.AllowanceWindow{}, false, true
	}
	if input.end <= 0 || input.end >= maxEpochMillis || input.start < 0 || input.start >= maxEpochMillis {
		return codemode.AllowanceWindow{}, false, false
	}
	reset := time.UnixMilli(input.end).UTC()
	window := codemode.AllowanceWindow{
		LimitID: "minimax", Window: input.name, Unit: codemode.AllowanceUnitPercent,
		Evidence: codemode.AllowanceEvidenceActive, UsedPercent: 100 - remaining,
		RemainingPercent: remaining, ResetsAt: &reset, ObservedAt: now,
	}
	if span := input.end - input.start; span > 0 && span%60000 == 0 {
		minutes := span / 60000
		window.WindowMinutes = &minutes
	}
	if window.Validate() != nil {
		return codemode.AllowanceWindow{}, false, false
	}
	return window, true, true
}

func requiredInt64(object oif.Value, name string) (int64, bool) {
	value, present, valid := optionalInt64(object, name)
	return value, valid && present
}

func optionalInt64(object oif.Value, name string) (int64, bool, bool) {
	value, present, valid := member(object, name)
	if !valid || !present {
		return 0, present, valid
	}
	if value.Kind() != oif.Number {
		return 0, true, false
	}
	parsed, err := strconv.ParseInt(value.Raw(), 10, 64)
	return parsed, true, err == nil
}

func optionalPercent(object oif.Value, name string) (float64, bool, bool) {
	value, present, valid := member(object, name)
	if !valid || !present {
		return 0, present, valid
	}
	if value.Kind() != oif.Number {
		return 0, true, false
	}
	parsed, err := strconv.ParseFloat(value.Raw(), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
		return 0, true, false
	}
	return parsed, true, true
}
