package resources

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

const (
	DefaultRetainedContainerMemoryTier = "1g"
	RetainedSubmissionMarkerField      = "olp_submission_id"
)

type metaField struct {
	key   string
	value any
}

func ValidRetainedContainerMemoryTier(tier string) bool {
	return tier == DefaultRetainedContainerMemoryTier || tier == "4g" || tier == "16g" || tier == "64g"
}

func RetainedFileMetadataFields() []string {
	return []string{"object", "purpose", "filename", "bytes", "status", "status_details", "expires_at", "created_at"}
}

func EnrichRetained(r *Resource, e retainedapi.Endpoint, source oif.Document, now time.Time) error {
	if r == nil {
		return ErrContract
	}
	enriched, err := enrichRetained(*r, e, source, now)
	if err != nil {
		return err
	}
	*r = enriched
	return nil
}

func enrichRetained(r Resource, e retainedapi.Endpoint, source oif.Document, now time.Time) (Resource, error) {
	if source.Root().Kind() != oif.Object {
		return Resource{}, ErrContract
	}
	meta := map[string]json.RawMessage{}
	if len(r.Metadata) > 0 && json.Unmarshal(r.Metadata, &meta) != nil {
		return Resource{}, ErrContract
	}
	root := source.Root()
	if err := enrichTimestamps(r.Kind, root, meta); err != nil {
		return Resource{}, err
	}
	if r.Kind == KindFile {
		for _, name := range RetainedFileMetadataFields() {
			if value, present := root.Lookup(name); present {
				meta[name] = json.RawMessage(value.Raw())
			}
		}
	}
	next, err := enrichStatus(r, root, now, meta)
	if err != nil {
		return Resource{}, err
	}
	next, err = enrichRetainedMeter(next, e, root, meta)
	if err != nil {
		return Resource{}, err
	}
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > metadataLimit {
		return Resource{}, ErrMetadataTooLarge
	}
	next.Metadata = raw
	return next, nil
}

func enrichTimestamps(kind string, root oif.Value, meta map[string]json.RawMessage) error {
	fields := []struct {
		native, stored string
		immutable      bool
	}{
		{"created_at", "created_at", true}, {"finished_at", "finished_at", true},
		{"createTime", "create_time", true}, {"endTime", "finished_at", true}, {"updateTime", "update_time", false},
	}
	for _, field := range fields {
		if kind == KindFile && field.native == "created_at" {
			continue
		}
		if err := enrichTimestampField(root, meta, field.native, field.stored, field.immutable); err != nil {
			return err
		}
	}
	return nil
}

func enrichTimestampField(root oif.Value, meta map[string]json.RawMessage, native, stored string, immutable bool) error {
	value, present := root.Lookup(native)
	if !present || value.Kind() == oif.Null {
		return nil
	}
	at, err := retainedTime(value)
	if err != nil {
		return err
	}
	formatted := at.UTC().Format(time.RFC3339Nano)
	if immutable {
		if err := verifyImmutableMeta(meta, stored, formatted, "immutable retained creation time changed"); err != nil {
			return err
		}
	}
	return putMeta(meta, metaField{stored, formatted})
}

func verifyImmutableMeta(meta map[string]json.RawMessage, key, want, message string) error {
	if _, exists := meta[key]; !exists {
		return nil
	}
	original, err := metaString(meta, key)
	if err != nil || original != want {
		return errors.New(message)
	}
	return nil
}

func enrichStatus(r Resource, root oif.Value, now time.Time, meta map[string]json.RawMessage) (Resource, error) {
	state := retainedState(root)
	if state == "" {
		return r, nil
	}
	previous, err := metaString(meta, "native_status")
	if err != nil {
		return Resource{}, err
	}
	if retainedTerminal(r.Kind, previous) && !retainedTerminal(r.Kind, state) {
		state = previous
	}
	if r.Kind != KindFile && r.State != StateDeleted && r.State != "submitting" && r.State != "submission_unknown" {
		r.State = state
	}
	if err := putMeta(meta, metaField{"native_status", state}); err != nil {
		return Resource{}, err
	}
	if !retainedTerminal(r.Kind, state) {
		return r, nil
	}
	if _, present := meta["terminal_observed_at"]; present {
		return r, nil
	}
	return r, putMeta(meta, metaField{"terminal_observed_at", now.UTC().Format(time.RFC3339Nano)})
}

func enrichRetainedMeter(r Resource, e retainedapi.Endpoint, root oif.Value, meta map[string]json.RawMessage) (Resource, error) {
	switch r.Kind {
	case "context_cache":
		if e.Action != "delete" {
			return enrichContextCache(r, root, meta)
		}
	case "vector_store":
		return enrichVectorStore(r, root, meta)
	case "container":
		return r, enrichContainer(root, meta)
	}
	return r, nil
}

func enrichContextCache(r Resource, root oif.Value, meta map[string]json.RawMessage) (Resource, error) {
	model, modelOK := retainedText(root, "model")
	pinned, err := metaString(meta, "serving_model")
	if err == nil && pinned == "" {
		pinned, err = metaString(meta, "upstream_model")
	}
	if err != nil {
		return Resource{}, err
	}
	if !modelOK || strings.TrimPrefix(model, "models/") != strings.TrimPrefix(pinned, "models/") {
		return Resource{}, errors.New("cache model does not match the admitted model")
	}
	tokens, err := retainedCount(retainedPath(root, "usageMetadata", "totalTokenCount"))
	if err != nil {
		return Resource{}, errors.New("cache response is missing authoritative token usage")
	}
	if previous, present := meta["total_token_count"]; present {
		var prior int64
		if json.Unmarshal(previous, &prior) != nil || prior != tokens {
			return Resource{}, errors.New("immutable cache quantity changed")
		}
	}
	expiry, err := retainedTime(retainedPath(root, "expireTime"))
	if err != nil {
		return Resource{}, errors.New("cache response is missing authoritative expiration")
	}
	r.ExpiresAt = &expiry
	return r, putMeta(meta, metaField{"model", model}, metaField{"total_token_count", tokens}, metaField{"expires_at", expiry.UTC().Format(time.RFC3339Nano)})
}

func enrichVectorStore(r Resource, root oif.Value, meta map[string]json.RawMessage) (Resource, error) {
	if quantity, present := root.Lookup("usage_bytes"); present && quantity.Kind() != oif.Null {
		bytes, err := retainedCount(quantity)
		if err != nil {
			return Resource{}, err
		}
		if err := putMeta(meta, metaField{"usage_bytes", bytes}); err != nil {
			return Resource{}, err
		}
	}
	value, present := root.Lookup("expires_at")
	if !present {
		return r, nil
	}
	if value.Kind() == oif.Null {
		r.ExpiresAt = nil
		return r, putMeta(meta, metaField{"expires_at", nil})
	}
	expiry, err := retainedTime(value)
	if err != nil {
		return Resource{}, err
	}
	r.ExpiresAt = &expiry
	return r, putMeta(meta, metaField{"expires_at", expiry.UTC().Format(time.RFC3339Nano)})
}

func enrichContainer(root oif.Value, meta map[string]json.RawMessage) error {
	tier, present := retainedText(root, "memory_limit")
	if !present {
		return nil
	}
	if !ValidRetainedContainerMemoryTier(tier) {
		return ErrContract
	}
	if err := verifyImmutableMeta(meta, "memory_tier", tier, "container memory tier differs from its admitted tier"); err != nil {
		return err
	}
	return putMeta(meta, metaField{"memory_tier", tier})
}

func putMeta(meta map[string]json.RawMessage, fields ...metaField) error {
	for _, field := range fields {
		raw, err := json.Marshal(field.value)
		if err != nil {
			return ErrContract
		}
		meta[field.key] = raw
	}
	return nil
}

func metaString(meta map[string]json.RawMessage, key string) (string, error) {
	raw, present := meta[key]
	if !present {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", ErrContract
	}
	return value, nil
}
