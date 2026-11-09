package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/oif"
)

type Count int64

func (c *Count) UnmarshalJSON(raw []byte) error {
	if isNull(raw) {
		return nil
	}
	text := string(raw)
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return errors.New("provider count must be a non-negative integer")
	}
	*c = Count(value)
	return nil
}

func (c *Count) Int64() *int64 {
	if c == nil {
		return nil
	}
	return new(int64(*c))
}

func (c *Count) orZero() Count {
	if c == nil {
		return 0
	}
	return *c
}

func sumCounts(counts ...Count) (int64, error) {
	total := int64(0)
	for _, count := range counts {
		if int64(count) > math.MaxInt64-total {
			return 0, errors.New("provider request counts overflow")
		}
		total += int64(count)
	}
	return total, nil
}

func UnixSeconds(raw json.RawMessage) (int64, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		at, err := time.Parse(time.RFC3339Nano, text)
		return at.Unix(), err == nil
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil {
		return 0, false
	}
	seconds, err := number.Float64()
	if err != nil || seconds < 0 || seconds >= math.MaxInt64 {
		return 0, false
	}
	return int64(seconds), true
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

func canonicalJSON(raw []byte) ([]byte, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: batches.MaxLineBytes}); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return json.Marshal(value)
}
