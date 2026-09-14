package usage

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// errStreamProtocol marks a Valkey stream reply that does not match the shape
// this consumer understands. Guessing at an unknown shape would mean guessing
// at which deliveries were handled, so every surprise is an error.
var errStreamProtocol = errors.New("unexpected request metadata stream reply")

func protocolError(reason string) error { return fmt.Errorf("%w: %s", errStreamProtocol, reason) }

// StreamEntry is one delivery from the request metadata stream. Exactly one of
// Payload and DeletedPendingID carries information: a normal delivery has the
// event payload, and a marker entry names a pending delivery the server
// dropped, which is loss that must be recorded rather than a payload to parse.
type StreamEntry struct {
	ID               string
	Payload          []byte
	DeletedPendingID string

	// milliseconds and sequence keep the parsed identifier so a page can be
	// ordered without re-parsing. Replies may arrive keyed by identifier, and
	// paging a recovery scan depends on knowing which entry is last.
	milliseconds uint64
	sequence     uint64
}

// autoClaimPage is one bounded page of a stale-delivery scan.
type autoClaimPage struct {
	NextStart  string
	Entries    []StreamEntry
	DeletedIDs []string
}

// ParseStreamID validates a Valkey stream identifier and returns its
// millisecond and sequence parts. Identifiers are used to name gaps and to
// resume scans, so a malformed one is rejected instead of being passed on.
func ParseStreamID(id string) (uint64, uint64, error) {
	milliseconds, sequence, found := strings.Cut(id, "-")
	if !found || milliseconds == "" || sequence == "" ||
		!asciiDigits(milliseconds) || !asciiDigits(sequence) {
		return 0, 0, protocolError("stream reply contained an invalid ID")
	}
	first, err := strconv.ParseUint(milliseconds, 10, 64)
	if err != nil {
		return 0, 0, protocolError("stream reply contained an overflowing ID")
	}
	second, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return 0, 0, protocolError("stream reply contained an overflowing ID")
	}
	return first, second, nil
}

func asciiDigits(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// parseReadReply reads an XREADGROUP reply, which arrives either as a one
// element array of [stream, entries] or, under RESP3, as a one entry map.
func parseReadReply(reply any, expectedStream string, batchSize int) ([]StreamEntry, error) {
	var name, entries any
	switch value := reply.(type) {
	case nil:
		return nil, nil
	case []any:
		if len(value) == 0 {
			return nil, nil
		}
		if len(value) != 1 {
			return nil, protocolError("invalid XREADGROUP reply")
		}
		pair, ok := value[0].([]any)
		if !ok || len(pair) != 2 {
			return nil, protocolError("invalid XREADGROUP stream tuple")
		}
		name, entries = pair[0], pair[1]
	case map[string]any:
		if len(value) != 1 {
			return nil, protocolError("invalid XREADGROUP reply")
		}
		for key, value := range value {
			name, entries = key, value
		}
	default:
		return nil, protocolError("invalid XREADGROUP reply")
	}
	stream, ok := name.(string)
	if !ok || stream != expectedStream {
		return nil, protocolError("XREADGROUP returned an unexpected stream")
	}
	return parseStreamEntries(entries, batchSize)
}

// parseAutoClaimReply reads the three element reply of the reclaim script: the
// cursor to resume from, the claimed entries, and the identifiers the server
// dropped from the pending list while scanning.
func parseAutoClaimReply(reply any, batchSize int) (*autoClaimPage, error) {
	items, ok := reply.([]any)
	if !ok {
		return nil, protocolError("invalid XAUTOCLAIM reply")
	}
	if len(items) != 3 {
		return nil, protocolError("invalid XAUTOCLAIM reply length")
	}
	nextStart, ok := items[0].(string)
	if !ok {
		return nil, protocolError("stream reply contained a non-string value")
	}
	if _, _, err := ParseStreamID(nextStart); err != nil {
		return nil, err
	}
	entries, err := parseStreamEntries(items[1], batchSize)
	if err != nil {
		return nil, err
	}
	deleted, err := parseIDList(items[2])
	if err != nil {
		return nil, err
	}
	if len(deleted) > batchSize*10 {
		return nil, protocolError("XAUTOCLAIM deleted-ID scan exceeded its protocol bound")
	}
	claimed := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		claimed[entry.ID] = struct{}{}
	}
	for _, id := range deleted {
		if _, overlaps := claimed[id]; overlaps {
			return nil, protocolError("XAUTOCLAIM returned overlapping claimed and deleted IDs")
		}
	}
	return &autoClaimPage{NextStart: nextStart, Entries: entries, DeletedIDs: deleted}, nil
}

// parseStreamEntries reads a page of entries, which arrives either as an array
// of [id, fields] tuples or, under RESP3, as a map keyed by identifier. An
// absent page is an empty one: a group read with nothing pending returns no
// entries, not an error.
func parseStreamEntries(value any, batchSize int) ([]StreamEntry, error) {
	var entries []StreamEntry
	switch page := value.(type) {
	case nil:
		return nil, nil
	case []any:
		if len(page) > batchSize {
			return nil, protocolError("stream reply exceeded the requested batch size")
		}
		entries = make([]StreamEntry, 0, len(page))
		seen := make(map[string]struct{}, len(page))
		for _, item := range page {
			tuple, ok := item.([]any)
			if !ok {
				return nil, protocolError("invalid stream entry tuple")
			}
			if len(tuple) != 2 {
				return nil, protocolError("invalid stream entry tuple length")
			}
			id, ok := tuple[0].(string)
			if !ok {
				return nil, protocolError("stream reply contained a non-string value")
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, protocolError("stream reply contained a duplicate entry ID")
			}
			seen[id] = struct{}{}
			entry, err := parseStreamEntry(id, tuple[1])
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	case map[string]any:
		if len(page) > batchSize {
			return nil, protocolError("stream reply exceeded the requested batch size")
		}
		entries = make([]StreamEntry, 0, len(page))
		for id, fields := range page {
			entry, err := parseStreamEntry(id, fields)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	default:
		return nil, protocolError("invalid stream entry list")
	}
	// A map reply carries no order, so impose the stream's own one: recovery
	// pages resume from the last identifier they handled.
	slices.SortFunc(entries, func(left, right StreamEntry) int {
		if order := cmp.Compare(left.milliseconds, right.milliseconds); order != 0 {
			return order
		}
		return cmp.Compare(left.sequence, right.sequence)
	})
	return entries, nil
}

// parseStreamEntry validates one identifier and the fields recorded under it.
func parseStreamEntry(id string, fields any) (StreamEntry, error) {
	milliseconds, sequence, err := ParseStreamID(id)
	if err != nil {
		return StreamEntry{}, err
	}
	payload, deleted, err := parseEntryFields(fields)
	if err != nil {
		return StreamEntry{}, err
	}
	return StreamEntry{
		ID:               id,
		Payload:          payload,
		DeletedPendingID: deleted,
		milliseconds:     milliseconds,
		sequence:         sequence,
	}, nil
}

// parseEntryFields reads the single field this stream writes. An entry with
// anything else carries no payload, which the caller records as loss rather
// than treating as an empty event.
func parseEntryFields(value any) ([]byte, string, error) {
	fields, err := entryFields(value)
	if err != nil {
		return nil, "", err
	}
	if len(fields) != 1 {
		return nil, "", nil
	}
	name, ok := fields[0].name.(string)
	if !ok {
		return nil, "", nil
	}
	switch name {
	case "event":
		payload, ok := fields[0].value.(string)
		if !ok {
			return nil, "", nil
		}
		return []byte(payload), "", nil
	case "deleted_pending_id":
		id, ok := fields[0].value.(string)
		if !ok {
			return nil, "", protocolError("stream reply contained a non-string value")
		}
		if _, _, err := ParseStreamID(id); err != nil {
			return nil, "", err
		}
		return nil, id, nil
	default:
		return nil, "", nil
	}
}

// streamField is one field recorded on a stream entry.
type streamField struct{ name, value any }

// entryFields flattens the three shapes a field container arrives in: a flat
// array of alternating names and values, an array of name and value pairs, or
// a map.
func entryFields(value any) ([]streamField, error) {
	switch container := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		fields := make([]streamField, 0, len(container))
		for name, item := range container {
			fields = append(fields, streamField{name, item})
		}
		return fields, nil
	case []any:
		if len(container) == 0 {
			return nil, nil
		}
		if _, paired := container[0].([]any); paired {
			fields := make([]streamField, 0, len(container))
			for _, item := range container {
				couple, ok := item.([]any)
				if !ok || len(couple) != 2 {
					return nil, protocolError("invalid stream field pair")
				}
				fields = append(fields, streamField{couple[0], couple[1]})
			}
			return fields, nil
		}
		if len(container)%2 != 0 {
			return nil, protocolError("stream field list has odd length")
		}
		fields := make([]streamField, 0, len(container)/2)
		for index := 0; index+1 < len(container); index += 2 {
			fields = append(fields, streamField{container[index], container[index+1]})
		}
		return fields, nil
	default:
		return nil, protocolError("invalid stream field container")
	}
}

func parseIDList(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, protocolError("invalid XAUTOCLAIM deleted-ID list")
	}
	ids := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, item := range list {
		id, ok := item.(string)
		if !ok {
			return nil, protocolError("stream reply contained a non-string value")
		}
		if _, _, err := ParseStreamID(id); err != nil {
			return nil, err
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, protocolError("XAUTOCLAIM returned a duplicate deleted ID")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}
