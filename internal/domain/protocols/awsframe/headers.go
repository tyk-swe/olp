// Package awsframe validates native AWS event framing before the SDK decoder
// can normalize duplicate headers or allocate from untrusted header lengths.
package awsframe

import (
	"encoding/binary"
	"errors"
)

func ValidateHeaders(data []byte) error {
	seen := map[string]bool{}
	invalid := errors.New("malformed or ambiguous AWS event headers")
	for len(data) > 0 {
		length := int(data[0])
		data = data[1:]
		if length == 0 || len(data) < length+1 {
			return invalid
		}
		name := string(data[:length])
		kind := data[length]
		data = data[length+1:]
		if seen[name] {
			return invalid
		}
		seen[name] = true
		if (name == ":message-type" || name == ":event-type" || name == ":exception-type" || name == ":content-type") && kind != 7 {
			return invalid
		}
		width := 0
		switch kind {
		case 0, 1:
		case 2:
			width = 1
		case 3:
			width = 2
		case 4:
			width = 4
		case 5, 8:
			width = 8
		case 9:
			width = 16
		case 6, 7:
			if len(data) < 2 {
				return invalid
			}
			width = 2 + int(binary.BigEndian.Uint16(data[:2]))
		default:
			return invalid
		}
		if len(data) < width {
			return invalid
		}
		data = data[width:]
	}
	return nil
}
