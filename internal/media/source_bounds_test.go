package media

import (
	"strings"
	"testing"
)

func TestMediaNonObjectSourceDoesNotAllocateAnIndex(t *testing.T) {
	body := []byte(" \n[" + strings.Repeat("0,", 1<<18) + "0]\t")
	var failure error
	allocations := testing.AllocsPerRun(2, func() {
		_, _, failure = sourceMediaFields(body)
	})
	if failure == nil || failure.Error() != "expected media object" || allocations > 8 {
		t.Fatalf("non-object envelope allocated an index: allocations=%v error=%v", allocations, failure)
	}
}

func TestMediaEnvelopeGuardPreservesObjectSource(t *testing.T) {
	body := []byte(" \n{\"prompt\":\"original\",\"native\":{\"number\":9007199254740993,\"zero\":-0}}\t")
	document, fields, err := sourceMediaFields(body)
	if err != nil || document.Raw() != string(body) || string(fields["native"]) != `{"number":9007199254740993,"zero":-0}` {
		t.Fatalf("valid media source was changed: document=%s error=%v", document.Raw(), err)
	}
	for _, input := range []string{"", "null", "true", "0", `"text"`, "[]", `{"x":0,"x":1}`, `{"x":`, `{} {}`} {
		if _, _, err := sourceMediaFields([]byte(input)); err == nil {
			t.Fatalf("accepted malformed media source %q", input)
		}
	}
}
