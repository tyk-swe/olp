package resources

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/usage"
)

func ObserveRetained(r *Resource, e retainedapi.Endpoint, source oif.Document, now time.Time) (RetainedObservation, error) {
	out := RetainedObservation{ObservedAt: now.UTC()}
	if r == nil || source.Root().Kind() != oif.Object {
		return out, ErrContract
	}
	root := source.Root()
	if e.Action == "delete" && RetainedDeletionTerminates(r.Kind) {
		out.RemoteTerminated = true
		return out, nil
	}
	state := retainedState(root)
	out.RemoteTerminated = retainedTerminal(r.Kind, state)
	if r.Kind == "vector_store" && state == "expired" && r.ExpiresAt != nil {
		out.ObservedAt = r.ExpiresAt.UTC()
	}
	if !out.RemoteTerminated || !isObservedJobKind(r.Kind) {
		return out, nil
	}
	start, end, model, models, err := retainedEvidenceContext(r)
	if err != nil {
		return out, err
	}
	out.ObservedAt = end
	if r.Kind == "eval_run" {
		return observeEvalRun(r, root, models, start, end, out)
	}
	return observeTraining(r.Kind, root, model, start, end, out)
}

func isObservedJobKind(kind string) bool {
	return kind == "fine_tuning_job" || kind == "vertex_tuning_job" || kind == "eval_run"
}

func observeTraining(kind string, root oif.Value, model string, start, end time.Time, base RetainedObservation) (RetainedObservation, error) {
	field := retainedPath(root, "trained_tokens")
	if kind == "vertex_tuning_job" {
		field = retainedPath(root, "tuningDataStats", "supervisedTuningDataStats", "totalBillableTokenCount")
	}
	if field.Kind() == oif.Null || field.Kind() == oif.Absent {
		return base, nil
	}
	tokens, err := retainedCount(field)
	if err != nil {
		return base, err
	}
	base.Lines = append(base.Lines, RetainedEvidenceLine{
		Component: "training", Model: model, Start: start, End: end,
		Usage: usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, UsageQuantities: usage.UsageQuantities{TrainingTokens: &tokens}},
	})
	base.CoverageComplete = true
	return base, nil
}

func observeEvalRun(r *Resource, root oif.Value, models []string, start, end time.Time, base RetainedObservation) (RetainedObservation, error) {
	perModel, present := root.Lookup("per_model_usage")
	if !present || perModel.Kind() == oif.Null {
		return base, nil
	}
	if perModel.Kind() != oif.Array {
		return base, ErrContract
	}
	var metadata struct {
		ServingModels map[string]string `json:"serving_models"`
	}
	if json.Unmarshal(r.Metadata, &metadata) != nil {
		return base, ErrContract
	}
	seen := map[string]bool{}
	for _, item := range perModel.Elements() {
		name, ok, err := resolveEvalModel(item, metadata.ServingModels)
		if err != nil {
			return base, err
		}
		if !ok || !slices.Contains(models, name) || seen[name] {
			return base, errors.New("evaluation usage names an unpinned or duplicate model")
		}
		seen[name] = true
		line, err := parseEvalUsageLine(item, name, start, end)
		if err != nil {
			return base, err
		}
		base.Lines = append(base.Lines, line)
	}
	base.CoverageComplete = len(seen) == len(models)
	return base, nil
}

func resolveEvalModel(item oif.Value, servingModels map[string]string) (string, bool, error) {
	name, ok := retainedText(item, "model_name")
	if !ok || len(servingModels) == 0 {
		return name, ok, nil
	}
	logical := ""
	for candidate, native := range servingModels {
		if native != name {
			continue
		}
		if logical != "" {
			return "", false, errors.New("evaluation usage has an ambiguous native model binding")
		}
		logical = candidate
	}
	return logical, logical != "", nil
}

func parseEvalUsageLine(item oif.Value, model string, start, end time.Time) (RetainedEvidenceLine, error) {
	input, inputErr := retainedCount(retainedPath(item, "prompt_tokens"))
	output, outputErr := retainedCount(retainedPath(item, "completion_tokens"))
	cached, cacheErr := retainedCount(retainedPath(item, "cached_tokens"))
	total, totalErr := retainedCount(retainedPath(item, "total_tokens"))
	invocations, invocationsErr := retainedCount(retainedPath(item, "invocation_count"))
	if inputErr != nil || outputErr != nil || cacheErr != nil || totalErr != nil || invocationsErr != nil || cached > input || input > math.MaxInt64-output || total != input+output {
		return RetainedEvidenceLine{}, ErrContract
	}
	return RetainedEvidenceLine{
		Component: "generation", Model: model, Start: start, End: end,
		Usage: usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, UsageQuantities: usage.UsageQuantities{InputTokens: &input, OutputTokens: &output, CachedInputTokens: &cached, InvocationCount: &invocations}},
	}, nil
}

func ObserveRetainedExpiry(r *Resource, now time.Time, remoteNotFound bool) (RetainedObservation, error) {
	out := RetainedObservation{ObservedAt: now.UTC()}
	if r == nil || r.Kind != "context_cache" || !remoteNotFound || r.ExpiresAt == nil || now.Before(*r.ExpiresAt) {
		return out, nil
	}
	model, tokens, start, end, err := parseCacheExpiryMeta(r)
	if err != nil {
		return out, err
	}
	if now.Before(end) {
		return out, nil
	}
	seconds := new(big.Int).Sub(big.NewInt(end.Unix()), big.NewInt(start.Unix()))
	nanos := new(big.Int).Add(new(big.Int).Mul(seconds, big.NewInt(1_000_000_000)), big.NewInt(int64(end.Nanosecond()-start.Nanosecond())))
	quantity := strings.TrimRight(strings.TrimRight(new(big.Rat).SetFrac(new(big.Int).Mul(nanos, big.NewInt(tokens)), big.NewInt(1_000_000_000)).FloatString(9), "0"), ".")
	out.Lines = []RetainedEvidenceLine{{
		Component: "cache_storage", Model: model, Start: start.UTC(), End: end.UTC(),
		Usage: usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, UsageQuantities: usage.UsageQuantities{CacheTokenSeconds: &quantity}},
	}}
	out.RemoteTerminated, out.CoverageComplete, out.ObservedAt = true, true, end.UTC()
	return out, nil
}

func parseCacheExpiryMeta(r *Resource) (string, int64, time.Time, time.Time, error) {
	var meta struct {
		Model      string `json:"upstream_model"`
		Tokens     *int64 `json:"total_token_count"`
		CreateTime string `json:"create_time"`
		ExpiresAt  string `json:"expires_at"`
	}
	if json.Unmarshal(r.Metadata, &meta) != nil || meta.Model == "" || meta.Tokens == nil || *meta.Tokens < 0 {
		return "", 0, time.Time{}, time.Time{}, ErrContract
	}
	start, startErr := time.Parse(time.RFC3339Nano, meta.CreateTime)
	end, endErr := time.Parse(time.RFC3339Nano, meta.ExpiresAt)
	if startErr != nil || endErr != nil || !end.Truncate(time.Microsecond).Equal(r.ExpiresAt.Truncate(time.Microsecond)) || !end.After(start) {
		return "", 0, time.Time{}, time.Time{}, ErrContract
	}
	return meta.Model, *meta.Tokens, start, end, nil
}

func retainedEvidenceContext(r *Resource) (time.Time, time.Time, string, []string, error) {
	var rawMeta struct {
		Model              string   `json:"upstream_model"`
		Models             []string `json:"authorized_models"`
		CreatedAt          string   `json:"created_at"`
		CreateTime         string   `json:"create_time"`
		FinishedAt         string   `json:"finished_at"`
		TerminalObservedAt string   `json:"terminal_observed_at"`
	}
	if json.Unmarshal(r.Metadata, &rawMeta) != nil || rawMeta.Model == "" {
		return time.Time{}, time.Time{}, "", nil, ErrContract
	}
	start, err := parseEvidenceStart(r.CreatedAt.UTC(), rawMeta.CreatedAt, rawMeta.CreateTime)
	if err != nil {
		return time.Time{}, time.Time{}, "", nil, ErrContract
	}
	end, err := parseEvidenceEnd(start, rawMeta.FinishedAt, rawMeta.TerminalObservedAt)
	if err != nil || start.IsZero() || !end.After(start) {
		return time.Time{}, time.Time{}, "", nil, ErrContract
	}
	return start, end, rawMeta.Model, rawMeta.Models, nil
}

func parseEvidenceStart(fallback time.Time, candidates ...string) (time.Time, error) {
	start := fallback
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, err
		}
		start = parsed.UTC()
	}
	return start, nil
}

func parseEvidenceEnd(start time.Time, finishedRaw, observedRaw string) (time.Time, error) {
	primary := finishedRaw
	if primary == "" {
		primary = observedRaw
	}
	end, err := time.Parse(time.RFC3339Nano, primary)
	if primary == "" || err != nil {
		return time.Time{}, ErrContract
	}
	end = end.UTC()
	if !end.After(start) && observedRaw != "" {
		// Integer-second provider clocks can report create == finish; fall back
		// to the first verified terminal observation to bound counter coverage.
		if observed, err := time.Parse(time.RFC3339Nano, observedRaw); err == nil && observed.After(start) {
			return observed.UTC(), nil
		}
	}
	return end, nil
}

func retainedState(root oif.Value) string {
	if state, ok := retainedText(root, "status"); ok {
		return state
	}
	state, _ := retainedText(root, "state")
	return state
}

func retainedTerminal(kind, state string) bool {
	switch kind {
	case "fine_tuning_job":
		return slices.Contains([]string{"succeeded", "failed", "cancelled"}, state)
	case "vertex_tuning_job":
		return slices.Contains([]string{"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}, state)
	case "eval_run":
		return slices.Contains([]string{"completed", "failed", "canceled", "cancelled"}, state)
	case "context_cache", "vector_store", "container":
		return state == "expired" || state == "deleted"
	}
	return false
}

func retainedPath(root oif.Value, path ...string) oif.Value {
	for _, name := range path {
		root, _ = root.Lookup(name)
	}
	return root
}

func retainedText(root oif.Value, key string) (string, bool) {
	field, present := root.Lookup(key)
	text, ok := field.Text()
	return text, present && ok && text != ""
}

func retainedCount(value oif.Value) (int64, error) {
	raw := value.Raw()
	if text, ok := value.Text(); ok {
		raw = text
	}
	count, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || count < 0 {
		return 0, ErrContract
	}
	return count, nil
}

func retainedTime(value oif.Value) (time.Time, error) {
	if raw, ok := value.Text(); ok {
		if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return at.UTC(), nil
		}
	}
	seconds, err := retainedCount(value)
	if err != nil || seconds == 0 {
		return time.Time{}, ErrContract
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func RetainedDeletionTerminates(kind string) bool {
	return kind == "vector_store" || kind == "container" || kind == "context_cache"
}
