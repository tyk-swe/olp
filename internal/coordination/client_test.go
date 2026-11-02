package coordination

import (
	"errors"
	"fmt"
	"testing"
)

func TestHasServerErrorCodeReadsTheCommandCause(t *testing.T) {
	for _, code := range []string{"BUSYGROUP", "NOGROUP"} {
		for _, suffix := range []string{"", ":"} {
			t.Run(code+suffix, func(t *testing.T) {
				err := fmt.Errorf("stream command: %w", &CommandError{Cause: errors.New(code + suffix + " server message")})
				if !HasServerErrorCode(err, code) {
					t.Fatal("server code in the command cause not detected")
				}
				if HasServerErrorCode(err, "WRONGTYPE") {
					t.Fatal("unrelated server code matched")
				}
			})
		}
	}
}

func TestHasServerErrorCodeNeedsAWholeToken(t *testing.T) {
	wrapped := fmt.Errorf("read group: %w", &CommandError{Cause: errors.New("NOGROUP No such key")})
	if !HasServerErrorCode(wrapped, "NOGROUP") {
		t.Fatal("wrapped server code not detected")
	}
	for _, message := range []string{"XNOGROUPX mentioned", "NOGROUPX: mentioned", "XNOGROUP: mentioned"} {
		if HasServerErrorCode(errors.New(message), "NOGROUP") {
			t.Fatalf("substring of a longer token matched: %q", message)
		}
	}
	if HasServerErrorCode(nil, "NOGROUP") {
		t.Fatal("nil error matched")
	}
	if HasServerErrorCode(&CommandError{}, "NOGROUP") {
		t.Fatal("command error without a cause matched")
	}
}
