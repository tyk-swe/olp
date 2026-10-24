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

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/usage"
)

// EnrichRetained records only billing and lifecycle facts. Native content,
// evaluation samples, training examples and conversation history remain remote.
func EnrichRetained(r *Resource, e retainedapi.Endpoint, source oif.Document, now time.Time) error {
	if r == nil || source.Root().Kind() != oif.Object {
		return ErrContract
	}
	meta := map[string]json.RawMessage{}
	if len(r.Metadata) > 0 && json.Unmarshal(r.Metadata, &meta) != nil {
		return ErrContract
	}
	put := func(name string, value any) { meta[name], _ = json.Marshal(value) }
	root := source.Root()
	for _, field := range []struct{ native, stored string }{{"created_at", "created_at"}, {"finished_at", "finished_at"}, {"createTime", "create_time"}, {"endTime", "finished_at"}, {"updateTime", "update_time"}} {
		if r.Kind == KindFile && field.native == "created_at" {
			continue
		}
		if value, present := root.Lookup(field.native); present && value.Kind() != oif.Null {
			at, err := retainedTime(value)
			if err != nil {
				return err
			}
			if field.stored == "created_at" || field.stored == "create_time" || field.stored == "finished_at" {
				if old, present := meta[field.stored]; present {
					var original string
					if json.Unmarshal(old, &original) != nil || original != at.UTC().Format(time.RFC3339Nano) {
						return errors.New("immutable retained creation time changed")
					}
				}
			}
			put(field.stored, at.UTC().Format(time.RFC3339Nano))
		}
	}
	if r.Kind == KindFile {
		for _, name := range []string{"object", "purpose", "filename", "bytes", "status", "status_details", "expires_at", "created_at"} {
			if value, present := root.Lookup(name); present {
				meta[name] = json.RawMessage(value.Raw())
			}
		}
	}
	state, _ := retainedText(root, "status")
	if state == "" {
		state, _ = retainedText(root, "state")
	}
	if state != "" {
		var previous string
		_ = json.Unmarshal(meta["native_status"], &previous)
		if retainedTerminal(r.Kind, previous) && !retainedTerminal(r.Kind, state) {
			state = previous
		}
		if r.Kind != KindFile && r.State != StateDeleted && r.State != "submitting" && r.State != "submission_unknown" {
			r.State = state
		}
		put("native_status", state)
		if retainedTerminal(r.Kind, state) {
			if _, present := meta["terminal_observed_at"]; !present {
				put("terminal_observed_at", now.UTC().Format(time.RFC3339Nano))
			}
		}
	}
	if r.Kind == "context_cache" && e.Action != "delete" {
		model, modelOK := retainedText(root, "model")
		var pinned string
		_ = json.Unmarshal(meta["serving_model"], &pinned)
		if pinned == "" {
			_ = json.Unmarshal(meta["upstream_model"], &pinned)
		}
		if !modelOK || strings.TrimPrefix(model, "models/") != strings.TrimPrefix(pinned, "models/") {
			return errors.New("cache model does not match the admitted model")
		}
		quantity := retainedPath(root, "usageMetadata", "totalTokenCount")
		tokens, err := retainedCount(quantity)
		if err != nil {
			return errors.New("cache response is missing authoritative token usage")
		}
		if previous, present := meta["total_token_count"]; present {
			var prior int64
			if json.Unmarshal(previous, &prior) != nil || prior != tokens {
				return errors.New("immutable cache quantity changed")
			}
		}
		expiry, err := retainedTime(retainedPath(root, "expireTime"))
		if err != nil {
			return errors.New("cache response is missing authoritative expiration")
		}
		put("model", model)
		put("total_token_count", tokens)
		put("expires_at", expiry.UTC().Format(time.RFC3339Nano))
		r.ExpiresAt = &expiry
	}
	if r.Kind == "vector_store" {
		if quantity, present := root.Lookup("usage_bytes"); present && quantity.Kind() != oif.Null {
			bytes, err := retainedCount(quantity)
			if err != nil {
				return err
			}
			put("usage_bytes", bytes)
		}
		if value, present := root.Lookup("expires_at"); present {
			if value.Kind() == oif.Null {
				r.ExpiresAt = nil
				put("expires_at", nil)
			} else {
				expiry, err := retainedTime(value)
				if err != nil {
					return err
				}
				r.ExpiresAt = &expiry
				put("expires_at", expiry.UTC().Format(time.RFC3339Nano))
			}
		}
	}
	if r.Kind == "container" {
		if tier, present := retainedText(root, "memory_limit"); present {
			if !slices.Contains([]string{"1g", "4g", "16g", "64g"}, tier) {
				return ErrContract
			}
			if old, present := meta["memory_tier"]; present {
				var original string
				if json.Unmarshal(old, &original) != nil || original != tier {
					return errors.New("container memory tier differs from its admitted tier")
				}
			}
			put("memory_tier", tier)
		}
	}
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > metadataLimit {
		return ErrMetadataTooLarge
	}
	r.Metadata = raw
	return nil
}

// ObserveRetained extracts complete native billing totals. Snapshots of bytes,
// cache tokens or container age are lifecycle facts, never billed intervals.
// Missing usage on failed/cancelled jobs leaves coverage incomplete.
func ObserveRetained(r *Resource, e retainedapi.Endpoint, source oif.Document, now time.Time) (RetainedObservation, error) {
	out := RetainedObservation{ObservedAt: now.UTC()}
	if r == nil || source.Root().Kind() != oif.Object {
		return out, ErrContract
	}
	root := source.Root()
	if e.Action == "delete" && slices.Contains([]string{"context_cache", "vector_store", "container"}, r.Kind) {
		out.RemoteTerminated = true
		return out, nil
	}
	state, _ := retainedText(root, "status")
	if state == "" {
		state, _ = retainedText(root, "state")
	}
	out.RemoteTerminated = retainedTerminal(r.Kind, state)
	if r.Kind == "vector_store" && state == "expired" && r.ExpiresAt != nil {
		out.ObservedAt = r.ExpiresAt.UTC()
	}
	if !out.RemoteTerminated || (r.Kind != "fine_tuning_job" && r.Kind != "vertex_tuning_job" && r.Kind != "eval_run") {
		return out, nil
	}
	start, end, model, models, err := retainedEvidenceContext(r, now)
	if err != nil {
		return out, err
	}
	out.ObservedAt = end
	if r.Kind == "fine_tuning_job" || r.Kind == "vertex_tuning_job" {
		field := retainedPath(root, "trained_tokens")
		if r.Kind == "vertex_tuning_job" {
			field = retainedPath(root, "tuningDataStats", "supervisedTuningDataStats", "totalBillableTokenCount")
		}
		if field.Kind() == oif.Null || field.Kind() == oif.Absent {
			return out, nil
		}
		tokens, err := retainedCount(field)
		if err != nil {
			return out, err
		}
		out.Lines = append(out.Lines, RetainedEvidenceLine{Component: "training", Model: model, Start: start, End: end, Usage: usage.AttemptUsage{Observed: true, Complete: true, TrainingTokens: &tokens}})
		out.CoverageComplete = true
		return out, nil
	}
	perModel, present := root.Lookup("per_model_usage")
	if !present || perModel.Kind() == oif.Null {
		return out, nil
	}
	if perModel.Kind() != oif.Array {
		return out, ErrContract
	}
	var metadata struct {
		ServingModels map[string]string `json:"serving_models"`
	}
	if json.Unmarshal(r.Metadata, &metadata) != nil {
		return out, ErrContract
	}
	seen := map[string]bool{}
	for _, item := range perModel.Elements() {
		name, ok := retainedText(item, "model_name")
		if ok && len(metadata.ServingModels) > 0 {
			logical := ""
			for candidate, native := range metadata.ServingModels {
				if native == name {
					if logical != "" {
						return out, errors.New("evaluation usage has an ambiguous native model binding")
					}
					logical = candidate
				}
			}
			name = logical
		}
		if !ok || !slices.Contains(models, name) || seen[name] {
			return out, errors.New("evaluation usage names an unpinned or duplicate model")
		}
		seen[name] = true
		input, inputErr := retainedCount(retainedPath(item, "prompt_tokens"))
		output, outputErr := retainedCount(retainedPath(item, "completion_tokens"))
		cached, cacheErr := retainedCount(retainedPath(item, "cached_tokens"))
		total, totalErr := retainedCount(retainedPath(item, "total_tokens"))
		invocations, invocationsErr := retainedCount(retainedPath(item, "invocation_count"))
		if inputErr != nil || outputErr != nil || cacheErr != nil || totalErr != nil || invocationsErr != nil || cached > input || input > math.MaxInt64-output || total != input+output {
			return out, ErrContract
		}
		out.Lines = append(out.Lines, RetainedEvidenceLine{Component: "generation", Model: name, Start: start, End: end, Usage: usage.AttemptUsage{Observed: true, Complete: true, InputTokens: &input, OutputTokens: &output, CachedInputTokens: &cached, InvocationCount: &invocations}})
	}
	// Every authorized model needs explicit native quantities, including zero
	// invocations. Omitted model usage remains an unresolved liability.
	out.CoverageComplete = len(seen) == len(models)
	return out, nil
}

// ObserveRetainedExpiry requires a verified remote 404 after the authoritative
// expiration. A cache's immutable token quantity and native create/expire times
// define the interval directly; no storage-size snapshots are interpolated.
func ObserveRetainedExpiry(r *Resource, now time.Time, remoteNotFound bool) (RetainedObservation, error) {
	out := RetainedObservation{ObservedAt: now.UTC()}
	if r == nil || r.Kind != "context_cache" || !remoteNotFound || r.ExpiresAt == nil || now.Before(*r.ExpiresAt) {
		return out, nil
	}
	var meta struct {
		Model      string `json:"upstream_model"`
		Tokens     *int64 `json:"total_token_count"`
		CreateTime string `json:"create_time"`
		ExpiresAt  string `json:"expires_at"`
	}
	if json.Unmarshal(r.Metadata, &meta) != nil || meta.Model == "" || meta.Tokens == nil || *meta.Tokens < 0 {
		return out, ErrContract
	}
	start, err := time.Parse(time.RFC3339Nano, meta.CreateTime)
	if err != nil {
		return out, ErrContract
	}
	end, err := time.Parse(time.RFC3339Nano, meta.ExpiresAt)
	if err != nil || !end.Truncate(time.Microsecond).Equal(r.ExpiresAt.Truncate(time.Microsecond)) || !end.After(start) {
		return out, ErrContract
	}
	if now.Before(end) {
		return out, nil
	}
	seconds := new(big.Int).Sub(big.NewInt(end.Unix()), big.NewInt(start.Unix()))
	nanos := new(big.Int).Add(new(big.Int).Mul(seconds, big.NewInt(1_000_000_000)), big.NewInt(int64(end.Nanosecond()-start.Nanosecond())))
	quantity := new(big.Rat).SetFrac(new(big.Int).Mul(nanos, big.NewInt(*meta.Tokens)), big.NewInt(1_000_000_000)).FloatString(9)
	quantity = strings.TrimRight(strings.TrimRight(quantity, "0"), ".")
	out.Lines = []RetainedEvidenceLine{{Component: "cache_storage", Model: meta.Model, Start: start.UTC(), End: end.UTC(), Usage: usage.AttemptUsage{Observed: true, Complete: true, CacheTokenSeconds: &quantity}}}
	out.RemoteTerminated, out.CoverageComplete = true, true
	out.ObservedAt = end.UTC()
	return out, nil
}

func retainedEvidenceContext(r *Resource, now time.Time) (time.Time, time.Time, string, []string, error) {
	var meta struct {
		Model              string   `json:"upstream_model"`
		Models             []string `json:"authorized_models"`
		CreatedAt          string   `json:"created_at"`
		CreateTime         string   `json:"create_time"`
		FinishedAt         string   `json:"finished_at"`
		TerminalObservedAt string   `json:"terminal_observed_at"`
	}
	if json.Unmarshal(r.Metadata, &meta) != nil || meta.Model == "" {
		return time.Time{}, time.Time{}, "", nil, ErrContract
	}
	start, end := r.CreatedAt.UTC(), now.UTC()
	for _, raw := range []string{meta.CreatedAt, meta.CreateTime} {
		if raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return start, end, "", nil, ErrContract
			}
			start = parsed.UTC()
		}
	}
	if raw := meta.FinishedAt; raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return start, end, "", nil, ErrContract
		}
		end = parsed.UTC()
	} else if raw := meta.TerminalObservedAt; raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return start, end, "", nil, ErrContract
		}
		end = parsed.UTC()
	} else {
		return start, end, "", nil, ErrContract
	}
	if !end.After(start) && meta.TerminalObservedAt != "" {
		// Integer-second provider clocks can report create == finish. A first
		// verified terminal observation bounds the counter's coverage; it never
		// derives tokens or money from elapsed time.
		if observed, err := time.Parse(time.RFC3339Nano, meta.TerminalObservedAt); err == nil && observed.After(start) {
			end = observed.UTC()
		}
	}
	if start.IsZero() || !end.After(start) {
		return start, end, "", nil, ErrContract
	}
	return start, end, meta.Model, meta.Models, nil
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
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err == nil {
			return at.UTC(), nil
		}
	}
	seconds, err := retainedCount(value)
	if err != nil || seconds == 0 {
		return time.Time{}, ErrContract
	}
	return time.Unix(seconds, 0).UTC(), nil
}

// Storage/container deletion stops the retained billing resource. A missing
// asynchronous job does not independently prove its paid execution terminated.
func RetainedDeletionTerminates(kind string) bool {
	return kind == "vector_store" || kind == "container" || kind == "context_cache"
}
