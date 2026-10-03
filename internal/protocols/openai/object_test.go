package openai

import (
	"reflect"
	"testing"
)

// object reads one object in one go and leaves the decoder, which copies its
// input into a buffer, to explain input that is not one. It must still give what
// the decoder gives, the value or the error, for any bytes.
func sameObject(t testing.TB, data []byte) {
	t.Helper()
	got, gotErr := object(data)
	want, wantErr := explainedObject(data)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotErr, wantErr) {
		t.Fatalf("object(%q) = %v, %v; the decoder reads %v, %v", data, got, gotErr, want, wantErr)
	}
}

func TestObjectReadsWhatTheDecoderReads(t *testing.T) {
	for _, data := range []string{
		``, ` `, `null`, ` null `, `{}`, ` { } `, `{"a":1}`, `{"a":1} `, "\n{\"a\":1}\n", `{"a":1}{"b":2}`, `{"a":1} x`, `{"a":1},`, `{"a":1`, `{"a"`, `[]`, `[{}]`, `"s"`, `1`, `true`,
		`{"a":1,"a":2}`, `{"a":{"b":[1,2,{"c":null}]},"d":"é😀"}`, `{"a":"\ud800"}`, "{\"a\":\"\xff\"}", `{"a":01}`, `{"a":1e999}`, `{"a":1.0000000000000000001}`,
		`{"":""}`, `{"a b":"c\nd"}`, "\xef\xbb\xbf{}", `{"a":1}` + "\x00", `{"a":nul}`, `{"a":[}`,
	} {
		sameObject(t, []byte(data))
	}
}

func FuzzObjectReadsWhatTheDecoderReads(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `{"a":1}{"b":2}`, `null`, `{"a":"😀"}`, `[1]`, ` {"x":{"y":[null]}} `} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) { sameObject(t, data) })
}
