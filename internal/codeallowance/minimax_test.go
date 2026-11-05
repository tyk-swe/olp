package codeallowance_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/codemode"
)

// Fixture fields are built from MiniMax-AI/cli@38aba5f QuotaModelRemain and
// test/fixtures/quota-response.json, fetched 2026-11-05. Bodies here are
// minimal constructions from that field set, not a copy of the fixture file.

const (
	remainsStart     = int64(1776355200000)
	remainsEnd       = remainsStart + 18000000
	remainsWeeklyEnd = remainsStart + 604800000
)

func minimaxBucket(model string, extra string) string {
	return fmt.Sprintf(`{"model_name":%q,"start_time":%d,"end_time":%d,"remains_time":18000000,`+
		`"current_interval_total_count":100,"current_interval_usage_count":80,"current_interval_remaining_percent":80,`+
		`"current_weekly_total_count":500,"current_weekly_usage_count":400,"current_weekly_remaining_percent":60,`+
		`"current_interval_status":1,"current_weekly_status":1,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":604800000%s}`,
		model, remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd, extra)
}

func minimaxBody(buckets ...string) string {
	body := `{"model_remains":[`
	for index, bucket := range buckets {
		if index > 0 {
			body += ","
		}
		body += bucket
	}
	return body + `],"base_resp":{"status_code":0,"status_msg":"success"}}`
}

func TestParseRemainsMapsServerPercentsToActiveWindows(t *testing.T) {
	allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(
		minimaxBucket("speech-hd", ""),
		minimaxBucket("MiniMax-M*", ""),
	)), nil, usageNow)
	if !ok {
		t.Fatal("pinned remains payload must parse")
	}
	primary := window(t, allowance, "minimax", "primary")
	if primary.Unit != codemode.AllowanceUnitPercent || primary.Evidence != codemode.AllowanceEvidenceActive ||
		primary.RemainingPercent != 80 || primary.UsedPercent != 20 ||
		primary.WindowMinutes == nil || *primary.WindowMinutes != 300 ||
		primary.ResetsAt == nil || !primary.ResetsAt.Equal(time.UnixMilli(remainsEnd).UTC()) ||
		!primary.ObservedAt.Equal(usageNow) {
		t.Fatalf("primary window = %+v", primary)
	}
	weekly := window(t, allowance, "minimax", "weekly")
	if weekly.RemainingPercent != 60 || weekly.UsedPercent != 40 ||
		weekly.WindowMinutes == nil || *weekly.WindowMinutes != 10080 ||
		weekly.ResetsAt == nil || !weekly.ResetsAt.Equal(time.UnixMilli(remainsWeeklyEnd).UTC()) {
		t.Fatalf("weekly window = %+v", weekly)
	}
	if len(allowance.Windows) != 2 || allowance.Credits != nil {
		t.Fatalf("allowance = %+v", allowance)
	}
	if err := allowance.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseRemainsGeneralBucketAndBoostNeverMultipliesWeekly(t *testing.T) {
	allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(
		minimaxBucket("general", `,"weekly_boost_permille":1500`),
	)), nil, usageNow)
	if !ok {
		t.Fatal("general coding bucket must parse")
	}
	if weekly := window(t, allowance, "minimax", "weekly"); weekly.RemainingPercent != 60 {
		t.Fatalf("weekly must use the base server percent, got %+v", weekly)
	}
}

func TestParseRemainsPrefersExactAccountModelWhenSeveralMatch(t *testing.T) {
	other := fmt.Sprintf(`{"model_name":"MiniMax-M2","start_time":%d,"end_time":%d,"remains_time":1,`+
		`"current_interval_total_count":10,"current_interval_usage_count":1,"current_interval_remaining_percent":10,`+
		`"current_weekly_total_count":0,"current_weekly_usage_count":0,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(other, minimaxBucket("MiniMax-M3", ""))), []string{"MiniMax-M3"}, usageNow)
	if !ok {
		t.Fatal("exact account model must disambiguate several coding buckets")
	}
	if primary := window(t, allowance, "minimax", "primary"); primary.RemainingPercent != 80 {
		t.Fatalf("selected bucket must be the exact account model, got %+v", primary)
	}
	if _, ok := codeallowance.ParseRemains([]byte(minimaxBody(other, minimaxBucket("MiniMax-M3", ""))), nil, usageNow); ok {
		t.Fatal("several coding buckets with no exact account model must write nothing")
	}
	if _, ok := codeallowance.ParseRemains([]byte(minimaxBody(minimaxBucket("speech-hd", ""), minimaxBucket("image-01", ""))), nil, usageNow); ok {
		t.Fatal("zero coding buckets must write nothing")
	}
}

func TestParseRemainsStatusAndWeeklyTotalZero(t *testing.T) {
	exhausted := fmt.Sprintf(`{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,`+
		`"current_interval_total_count":100,"current_interval_usage_count":100,"current_interval_status":2,`+
		`"current_weekly_total_count":0,"current_weekly_usage_count":0,"current_weekly_status":3,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(exhausted)), nil, usageNow)
	if !ok {
		t.Fatal("status 2 must map to an exhausted window without a server percent")
	}
	if len(allowance.Windows) != 1 {
		t.Fatalf("status 3 weekly window must be skipped, got %+v", allowance.Windows)
	}
	if primary := window(t, allowance, "minimax", "primary"); primary.RemainingPercent != 0 || primary.UsedPercent != 100 {
		t.Fatalf("exhausted primary = %+v", primary)
	}

	notInPlan := fmt.Sprintf(`{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,`+
		`"current_interval_total_count":0,"current_interval_usage_count":0,"current_interval_status":3,`+
		`"current_weekly_total_count":0,"current_weekly_usage_count":0,"current_weekly_status":3,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(notInPlan)), nil, usageNow); ok {
		t.Fatalf("a bucket outside the plan must write nothing, got %+v", allowance)
	}

	weeklyZeroNoPercent := fmt.Sprintf(`{"model_name":"MiniMax-M*","start_time":%d,"end_time":%d,"remains_time":1,`+
		`"current_interval_total_count":100,"current_interval_usage_count":20,"current_interval_remaining_percent":80,`+
		`"current_weekly_total_count":0,"current_weekly_usage_count":0,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	allowance, ok = codeallowance.ParseRemains([]byte(minimaxBody(weeklyZeroNoPercent)), nil, usageNow)
	if !ok || len(allowance.Windows) != 1 {
		t.Fatalf("weekly total 0 with no weekly percent must keep only the interval window, got %+v ok %v", allowance, ok)
	}
}

func TestParseRemainsCountsNeverMapToAPercent(t *testing.T) {
	countsOnly := fmt.Sprintf(`{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,`+
		`"current_interval_total_count":100,"current_interval_usage_count":20,`+
		`"current_weekly_total_count":500,"current_weekly_usage_count":100,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(countsOnly)), nil, usageNow); ok {
		t.Fatalf("counts without a server percent or exhausted status must write nothing, got %+v", allowance)
	}
}

func TestParseRemainsRejectsMalformed(t *testing.T) {
	valid := minimaxBucket("general", "")
	cases := map[string]string{
		"not json":              `{`,
		"root not object":       `[]`,
		"missing model_remains": `{"base_resp":{"status_code":0,"status_msg":"ok"}}`,
		"model_remains object":  `{"model_remains":{}}`,
		"model_remains null":    `{"model_remains":null}`,
		"model_remains empty":   `{"model_remains":[]}`,
		"base_resp nonzero":     `{"model_remains":[` + valid + `],"base_resp":{"status_code":1002,"status_msg":"no"}}`,
		"base_resp string code": `{"model_remains":[` + valid + `],"base_resp":{"status_code":"0","status_msg":"ok"}}`,
		"base_resp float code":  `{"model_remains":[` + valid + `],"base_resp":{"status_code":0.5,"status_msg":"ok"}}`,
		"percent string": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":"80","current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"percent over 100": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":101,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"weekly percent over 100": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_weekly_total_count":1,"current_weekly_usage_count":0,"current_weekly_remaining_percent":150,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"percent negative": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":-1,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"end_time zero": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":0,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsStart, remainsWeeklyEnd),
		"end_time string": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":"%d","remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"end_time float": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d.5,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"total string": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":"100","current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"status string": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_interval_status":"1","current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"status unknown": fmt.Sprintf(`{"model_remains":[{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_interval_status":9,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd),
		"boost string":      minimaxBody(minimaxBucket("general", `,"weekly_boost_permille":"1500"`)),
		"model_name number": `{"model_remains":[{"model_name":3}]}`,
		"bucket not object": `{"model_remains":["general"]}`,
		"missing start_time": fmt.Sprintf(`{"model_remains":[{"model_name":"general","end_time":%d,"remains_time":1,"current_interval_total_count":100,"current_interval_usage_count":1,"current_interval_remaining_percent":80,"current_weekly_total_count":1,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}]}`,
			remainsEnd, remainsStart, remainsWeeklyEnd),
	}
	for name, body := range cases {
		if allowance, ok := codeallowance.ParseRemains([]byte(body), nil, usageNow); ok {
			t.Errorf("%s must write nothing, got %+v", name, allowance)
		}
	}
}

func TestParseRemainsMalformedRowAnywhereWritesNothing(t *testing.T) {
	brokenType := fmt.Sprintf(`{"model_name":"speech-hd","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":"not-a-count","current_interval_usage_count":0,"current_weekly_total_count":0,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(brokenType, minimaxBucket("general", ""))), nil, usageNow); ok {
		t.Fatalf("a malformed required field on an unselected row must write nothing, got %+v", allowance)
	}
	brokenMissing := `{"model_name":"speech-hd"}`
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(brokenMissing, minimaxBucket("general", ""))), nil, usageNow); ok {
		t.Fatalf("a row missing required numeric fields must write nothing, got %+v", allowance)
	}
	brokenPercent := fmt.Sprintf(`{"model_name":"speech-hd","start_time":%d,"end_time":%d,"remains_time":1,"current_interval_total_count":1,"current_interval_usage_count":0,"current_interval_remaining_percent":"80","current_weekly_total_count":0,"current_weekly_usage_count":0,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(brokenPercent, minimaxBucket("general", ""))), nil, usageNow); ok {
		t.Fatalf("a mistyped percent on an unselected row must write nothing, got %+v", allowance)
	}
}

func TestParseRemainsStatus3UsesValidServerPercent(t *testing.T) {
	unlimited := fmt.Sprintf(`{"model_name":"general","start_time":%d,"end_time":%d,"remains_time":1,`+
		`"current_interval_total_count":0,"current_interval_usage_count":0,"current_interval_remaining_percent":80,"current_interval_status":3,`+
		`"current_weekly_total_count":0,"current_weekly_usage_count":0,"current_weekly_remaining_percent":60,"current_weekly_status":3,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":1}`,
		remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd)
	allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(unlimited)), nil, usageNow)
	if !ok {
		t.Fatal("status 3 with a valid server percent must use that percent")
	}
	if primary := window(t, allowance, "minimax", "primary"); primary.RemainingPercent != 80 {
		t.Fatalf("primary = %+v", primary)
	}
	if weekly := window(t, allowance, "minimax", "weekly"); weekly.RemainingPercent != 60 {
		t.Fatalf("weekly = %+v", weekly)
	}
}

func TestParseRemainsSelectionNarrowsToEvidence(t *testing.T) {
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(minimaxBucket("MiniMax-M9", ""))), nil, usageNow); ok {
		t.Fatalf("an arbitrary MiniMax-M prefix name must not qualify on prefix alone, got %+v", allowance)
	}
	allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(minimaxBucket("MiniMax-M9", ""))), []string{"MiniMax-M9"}, usageNow)
	if !ok {
		t.Fatal("an exact account model starting with MiniMax-M must qualify")
	}
	if primary := window(t, allowance, "minimax", "primary"); primary.RemainingPercent != 80 {
		t.Fatalf("primary = %+v", primary)
	}
	if allowance, ok := codeallowance.ParseRemains([]byte(minimaxBody(minimaxBucket("speech-hd", ""))), []string{"speech-hd"}, usageNow); ok {
		t.Fatalf("an exact account model that is not MiniMax-M must not qualify, got %+v", allowance)
	}
	if _, ok := codeallowance.ParseRemains([]byte(minimaxBody(minimaxBucket("MiniMax-M*", ""))), nil, usageNow); !ok {
		t.Fatal("the literal MiniMax-M* bucket must qualify")
	}
	if _, ok := codeallowance.ParseRemains([]byte(minimaxBody(minimaxBucket("general", ""))), nil, usageNow); !ok {
		t.Fatal("the general bucket must qualify")
	}
}
