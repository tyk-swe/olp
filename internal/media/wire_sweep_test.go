package media

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranscriptionJSONRoundTripsSegmentIDs(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantID string
	}{
		{"diarized string id",
			`{"text":"hi","segments":[{"type":"transcript.text.segment","id":"seg_001","start":0,"end":1.5,"text":"hi","speaker":"A"}]}`,
			`"seg_001"`},
		{"verbose integer id",
			`{"text":"hi","segments":[{"id":0,"start":0,"end":1.5,"text":"hi"}]}`,
			`0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, failure := DecodeTranscriptionJSON([]byte(tc.body))
			if failure != nil {
				t.Fatal(failure)
			}
			if len(result.Segments) != 1 || string(result.Segments[0].ID) != tc.wantID {
				t.Fatalf("segments %+v", result.Segments)
			}
			body, failure := EncodeTranscriptionJSON(result)
			if failure != nil {
				t.Fatal(failure)
			}
			var doc struct {
				Segments []map[string]json.RawMessage `json:"segments"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Segments) != 1 || string(doc.Segments[0]["id"]) != tc.wantID {
				t.Fatalf("encoded %s", body)
			}
			if strings.Contains(tc.body, "speaker") && (string(doc.Segments[0]["speaker"]) != `"A"` ||
				string(doc.Segments[0]["type"]) != `"transcript.text.segment"`) {
				t.Fatalf("encoded %s", body)
			}
		})
	}
}
