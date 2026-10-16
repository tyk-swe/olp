package fidelity_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/tests/fidelity"
)

// This digest pins the fixture contracts and every referenced artifact together.

func TestOracleRejectsUnicodeRepairAndPrecisionLoss(t *testing.T) {
	for _, corrupted := range [][]byte{
		[]byte(`{"opaque":"\ud800"}`),
		[]byte(`{"opaque":"\udc00"}`),
		[]byte(`{"opaque":"\ud800\u0041"}`),
		append(append([]byte(`{"opaque":"`), 0xff), []byte(`"}`)...),
	} {
		if fidelity.Compare([]byte(`{"opaque":"�"}`), corrupted) == nil {
			t.Fatal("Unicode repair hid corruption")
		}
		if _, err := fidelity.Decode(corrupted); err == nil {
			t.Fatal("ambiguous Unicode accepted")
		}
	}
	for _, valid := range []string{`{"opaque":"\ud83d\ude00"}`, `{"opaque":"\\ud800"}`, `{"opaque":"�"}`} {
		if _, err := fidelity.Decode([]byte(valid)); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`},
		{`{"n":1.0000000000000001}`, `{"n":1.0000000000000002}`},
		{`{"n":1e400}`, `{"n":1e399}`},
		{`{"opaque":"é"}`, `{"opaque":"é"}`},
	} {
		if fidelity.Compare([]byte(pair[0]), []byte(pair[1])) == nil {
			t.Fatal("required precision or opaque representation lost")
		}
	}
}

func FuzzReferenceRetainsOpaqueState(f *testing.F) {
	for _, seed := range []string{"signature", "", "é", "\u0000", "😀", "9007199254740993"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		original, _ := json.Marshal(map[string]any{"state": value, "ordered": []int{0, 1, 2}})
		if err := fidelity.Compare(original, original); err != nil {
			t.Fatal(err)
		}
		changed, _ := json.Marshal(map[string]any{"state": value + "!", "ordered": []int{0, 1, 2}})
		if fidelity.Compare(original, changed) == nil {
			t.Fatal("changed opaque state passed")
		}
		reordered, _ := json.Marshal(map[string]any{"state": value, "ordered": []int{0, 2, 1}})
		if fidelity.Compare(original, reordered) == nil {
			t.Fatal("changed order passed")
		}
	})
}
