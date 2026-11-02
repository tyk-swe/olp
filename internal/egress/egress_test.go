package egress

import (
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestIsRedirectThroughWrapping(t *testing.T) {
	wrapped := &url.Error{Op: "Get", URL: "https://provider.example", Err: ErrRedirect}
	if !IsRedirect(wrapped) {
		t.Fatal("wrapped redirect refusal not detected")
	}
	if !IsRedirect(fmt.Errorf("attempt failed: %w", wrapped)) {
		t.Fatal("doubly wrapped redirect refusal not detected")
	}
}

func TestIsRedirectThroughFlattenedText(t *testing.T) {
	flattened := errors.New("Get https://provider.example: upstream redirects are refused")
	if !IsRedirect(flattened) {
		t.Fatal("flattened redirect refusal not detected")
	}
}

func TestIsRedirectRejectsOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("connection reset"), ErrUnsafeDestination} {
		if IsRedirect(err) {
			t.Fatalf("%v reported as a redirect refusal", err)
		}
	}
}
