package usage

import (
	"errors"
	"strconv"
	"testing"
)

func TestParseStreamIDRejectsMalformedIdentifiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id                     string
		milliseconds, sequence uint64
		valid                  bool
	}{
		{id: "1700000000000-0", milliseconds: 1700000000000, valid: true},
		{id: "1-2", milliseconds: 1, sequence: 2, valid: true},
		{id: "0-0", valid: true},
		{id: "1700000000000"},
		{id: "-0"},
		{id: "1-"},
		{id: "1-x"},
		{id: "-1-2"},
		{id: " 1-2"},
		{id: "99999999999999999999-0"},
		{id: ""},
	}
	for _, test := range cases {
		t.Run(test.id, func(t *testing.T) {
			t.Parallel()
			milliseconds, sequence, err := ParseStreamID(test.id)
			if test.valid {
				if err != nil {
					t.Fatalf("ParseStreamID(%q) = %v", test.id, err)
				}
				if milliseconds != test.milliseconds || sequence != test.sequence {
					t.Fatalf("ParseStreamID(%q) = %d-%d", test.id, milliseconds, sequence)
				}
				return
			}
			if !errors.Is(err, errStreamProtocol) {
				t.Fatalf("ParseStreamID(%q) = %v, want a protocol error", test.id, err)
			}
		})
	}
}

func ingestEntry(id string, fields ...any) []any { return []any{id, fields} }

func TestParseReadReplyAcceptsBothProtocolShapes(t *testing.T) {
	t.Parallel()
	payload := `{"version":1}`
	cases := []struct {
		name    string
		reply   any
		entries []StreamEntry
		invalid bool
	}{
		{name: "nothing to read", reply: nil},
		{name: "empty page", reply: map[string]any{"stream": []any{}}},
		{name: "resp2 array", reply: []any{[]any{"stream",
			[]any{ingestEntry("1-1", "event", payload)}}},
			entries: []StreamEntry{{ID: "1-1", Payload: []byte(payload)}}},
		{name: "resp3 map", reply: map[string]any{"stream": []any{ingestEntry("1-1", "event", payload)}},
			entries: []StreamEntry{{ID: "1-1", Payload: []byte(payload)}}},
		{name: "deleted marker", reply: map[string]any{"stream": []any{
			ingestEntry("2-0", "deleted_pending_id", "1-9")}},
			entries: []StreamEntry{{ID: "2-0", DeletedPendingID: "1-9"}}},
		{name: "unknown field carries nothing", reply: map[string]any{"stream": []any{
			ingestEntry("2-0", "other", payload)}},
			entries: []StreamEntry{{ID: "2-0"}}},
		{name: "two fields carry nothing", reply: map[string]any{"stream": []any{
			ingestEntry("2-0", "event", payload, "other", "x")}},
			entries: []StreamEntry{{ID: "2-0"}}},
		{name: "nil field list carries nothing", reply: map[string]any{"stream": []any{
			[]any{"2-0", nil}}},
			entries: []StreamEntry{{ID: "2-0"}}},
		{name: "another stream", reply: map[string]any{"other": []any{}}, invalid: true},
		{name: "two streams", reply: map[string]any{"stream": []any{}, "other": []any{}}, invalid: true},
		{name: "wrong tuple length", reply: []any{[]any{"stream"}}, invalid: true},
		{name: "not a stream reply", reply: int64(1), invalid: true},
		{name: "entry is not a tuple", reply: map[string]any{"stream": []any{"1-1"}}, invalid: true},
		{name: "entry id is not a string", reply: map[string]any{"stream": []any{
			[]any{int64(1), []any{}}}}, invalid: true},
		{name: "malformed entry id", reply: map[string]any{"stream": []any{
			ingestEntry("nope", "event", payload)}}, invalid: true},
		{name: "odd field list", reply: map[string]any{"stream": []any{
			[]any{"1-1", []any{"event"}}}}, invalid: true},
		{name: "duplicate entry", reply: map[string]any{"stream": []any{
			ingestEntry("1-1", "event", payload), ingestEntry("1-1", "event", payload)}}, invalid: true},
		{name: "malformed deleted marker", reply: map[string]any{"stream": []any{
			ingestEntry("2-0", "deleted_pending_id", "nope")}}, invalid: true},
		{name: "over batch size", reply: map[string]any{"stream": []any{
			ingestEntry("1-1", "event", payload), ingestEntry("1-2", "event", payload),
			ingestEntry("1-3", "event", payload)}}, invalid: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			entries, err := parseReadReply(test.reply, "stream", 2)
			if test.invalid {
				if !errors.Is(err, errStreamProtocol) {
					t.Fatalf("parseReadReply = %v, want a protocol error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReadReply: %v", err)
			}
			if len(entries) != len(test.entries) {
				t.Fatalf("got %d entries, want %d", len(entries), len(test.entries))
			}
			for index, want := range test.entries {
				got := entries[index]
				if got.ID != want.ID || string(got.Payload) != string(want.Payload) ||
					got.DeletedPendingID != want.DeletedPendingID {
					t.Fatalf("entry %d = %+v, want %+v", index, got, want)
				}
			}
		})
	}
}

func TestParseAutoClaimReplyBoundsWhatItTrusts(t *testing.T) {
	t.Parallel()
	entry := ingestEntry("1-1", "event", `{"version":1}`)
	cases := []struct {
		name             string
		reply            any
		nextStart        string
		entries, deleted int
		invalid          bool
	}{
		{name: "claimed page", reply: []any{"0-0", []any{entry}, []any{"9-9"}},
			nextStart: "0-0", entries: 1, deleted: 1},
		{name: "empty page", reply: []any{"0-0", nil, nil}, nextStart: "0-0"},
		{name: "not an array", reply: "0-0", invalid: true},
		{name: "wrong length", reply: []any{"0-0", nil}, invalid: true},
		{name: "malformed cursor", reply: []any{"nope", nil, nil}, invalid: true},
		{name: "cursor is not a string", reply: []any{int64(0), nil, nil}, invalid: true},
		{name: "malformed deleted id", reply: []any{"0-0", nil, []any{"nope"}}, invalid: true},
		{name: "duplicate deleted id", reply: []any{"0-0", nil, []any{"9-9", "9-9"}}, invalid: true},
		{name: "claimed and deleted overlap", reply: []any{"0-0", []any{entry}, []any{"1-1"}}, invalid: true},
		{name: "deleted scan beyond its bound",
			reply: []any{"0-0", nil, ingestManyIDs(21)}, invalid: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			page, err := parseAutoClaimReply(test.reply, 2)
			if test.invalid {
				if !errors.Is(err, errStreamProtocol) {
					t.Fatalf("parseAutoClaimReply = %v, want a protocol error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAutoClaimReply: %v", err)
			}
			if page.NextStart != test.nextStart || len(page.Entries) != test.entries ||
				len(page.DeletedIDs) != test.deleted {
				t.Fatalf("page = %+v", page)
			}
		})
	}
}

func ingestManyIDs(count int) []any {
	ids := make([]any, 0, count)
	for index := range count {
		ids = append(ids, "9-"+strconv.Itoa(index))
	}
	return ids
}
