package usage

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
	}{
		{"nanosecond precision", time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)},
		{"whole second", time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"non-utc zone", time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("east", 3*60*60))},
	}
	id := "0195f3c2-6a1e-7c8d-9e0f-1a2b3c4d5e6f"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, decoded, err := DecodeCursor(EncodeCursor(tc.at, id))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !at.Equal(tc.at) {
				t.Errorf("at = %s, want %s", at, tc.at)
			}
			if at.Location() != time.UTC {
				t.Errorf("location = %s, want UTC", at.Location())
			}
			if decoded != id {
				t.Errorf("id = %s, want %s", decoded, id)
			}
		})
	}
}

func TestDecodeCursorRejectsMalformedTokens(t *testing.T) {
	id := "0195f3c2-6a1e-7c8d-9e0f-1a2b3c4d5e6f"
	valid := EncodeCursor(time.Now(), id)
	cases := []struct {
		name   string
		cursor string
	}{
		{"empty", ""},
		{"not base64", "!!!!"},
		{"padded base64", base64.URLEncoding.EncodeToString([]byte("2026-03-04T05:06:07.5Z|" + id))},
		{"trailing padding", valid + "=="},
		{"non-url alphabet", "MjAyNi0wMy0wNFQwNTowNjowN1o+"},
		{"no separator", base64.RawURLEncoding.EncodeToString([]byte("2026-03-04T05:06:07Z"))},
		{"timestamp is not rfc3339", base64.RawURLEncoding.EncodeToString([]byte("yesterday|" + id))},
		{"id is not a uuid", base64.RawURLEncoding.EncodeToString([]byte("2026-03-04T05:06:07Z|next"))},
		{"empty id", base64.RawURLEncoding.EncodeToString([]byte("2026-03-04T05:06:07Z|"))},
		{"truncated", valid[:len(valid)-4]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, decoded, err := DecodeCursor(tc.cursor)
			if err == nil {
				t.Fatalf("accepted %s (%s, %s)", tc.name, at, decoded)
			}
			if !at.IsZero() || decoded != "" {
				t.Errorf("rejected cursor returned %s and %q", at, decoded)
			}
		})
	}
}
