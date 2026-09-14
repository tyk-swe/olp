package usage

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errMalformedCursor = errors.New("malformed cursor")

// EncodeCursor renders the position of one row in a list ordered by timestamp
// and identifier. The cursor is opaque on purpose: it is a resume token, not an
// offset, so a page stays stable while rows are inserted ahead of it.
func EncodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// DecodeCursor reads a cursor produced by EncodeCursor. A cursor that was
// tampered with, truncated or invented is rejected rather than clamped: it
// would otherwise silently page from somewhere the caller never asked for.
func DecodeCursor(s string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", errMalformedCursor
	}
	timestamp, id, found := strings.Cut(string(raw), "|")
	if !found {
		return time.Time{}, "", errMalformedCursor
	}
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return time.Time{}, "", errMalformedCursor
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, "", errMalformedCursor
	}
	return at.UTC(), parsed.String(), nil
}
