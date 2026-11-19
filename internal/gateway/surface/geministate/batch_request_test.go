package geministate

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/resources"
)

const generate = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

func inline(items ...string) string {
	return `{"batch":{"input_config":{"requests":{"requests":[` + strings.Join(items, ",") + `]}}}}`
}

func item(key string) string {
	return `{"request":` + generate + `,"metadata":{"key":"` + key + `"}}`
}

func TestBatchRequestsAreAcceptedOnlyInTheTwoInputShapesAndTranslatedForTheProvider(t *testing.T) {
	t.Parallel()
	file := "files/" + resources.LocalID(resources.KindGeminiFile, uuid.New())

	accepted, err := validateBatch([]byte(`{"batch":{"display_name":"nightly","inputConfig":{"requests":{"requests":[`+item("a")+`,`+item("b")+`]}}}}`), "route")
	if err != nil {
		t.Fatalf("inline batch refused: %+v", err)
	}
	var sent struct {
		Batch struct {
			DisplayName string
			InputConfig struct {
				FileName string
				Requests struct{ Requests []json.RawMessage }
			}
		}
	}
	if json.Unmarshal(accepted.Upstream, &sent) != nil || sent.Batch.DisplayName != "nightly" || len(sent.Batch.InputConfig.Requests.Requests) != 2 {
		t.Fatalf("provider body = %s, want the display name and both requests", accepted.Upstream)
	}
	if accepted.Expected == nil || *accepted.Expected != 2 || !slices.Equal(accepted.Keys, []string{"a", "b"}) || accepted.LocalFileID != "" {
		t.Fatalf("inline batch = %+v, want two keyed requests and no file", accepted)
	}

	bound, err := validateBatch([]byte(`{"batch":{"input_config":{"file_name":"`+file+`"}}}`), "route")
	if err != nil {
		t.Fatalf("file batch refused: %+v", err)
	}
	if json.Unmarshal(bound.Upstream, &sent) != nil || sent.Batch.InputConfig.FileName != file {
		t.Fatalf("provider body = %s, want the file name", bound.Upstream)
	}
	if bound.Expected != nil || bound.Keys != nil || "files/"+bound.LocalFileID != file || bound.UpstreamFile != file || bound.DisplayName != nil {
		t.Fatalf("file batch = %+v, want only the caller's file", bound)
	}
	respelled, err := validateBatch([]byte(`{ "batch" : {"input_config":{"file_name":"`+file+`"}} }`), "route")
	if err != nil || string(respelled.Canonical) != string(bound.Canonical) {
		t.Fatalf("the same request spelled with other whitespace has another canonical form: %s and %s", respelled.Canonical, bound.Canonical)
	}

	for name, body := range map[string]string{
		"not an object":                `[]`,
		"field beside batch":           `{"batch":{"input_config":{"file_name":"` + file + `"}},"model":"x"}`,
		"batch is not an object":       `{"batch":"x"}`,
		"unsupported batch field":      `{"batch":{"priority":1,"input_config":{"file_name":"` + file + `"}}}`,
		"both display name spellings":  `{"batch":{"display_name":"a","displayName":"b","input_config":{"file_name":"` + file + `"}}}`,
		"display name is not a string": `{"batch":{"display_name":1,"input_config":{"file_name":"` + file + `"}}}`,
		"display name is too long":     `{"batch":{"display_name":"` + strings.Repeat("n", 513) + `","input_config":{"file_name":"` + file + `"}}}`,
		"no input":                     `{"batch":{"display_name":"a"}}`,
		"both input spellings":         `{"batch":{"input_config":{"file_name":"` + file + `"},"inputConfig":{"file_name":"` + file + `"}}}`,
		"input is not an object":       `{"batch":{"input_config":"x"}}`,
		"two input sources":            `{"batch":{"input_config":{"file_name":"` + file + `","requests":{"requests":[` + item("a") + `]}}}}`,
		"unknown input source":         `{"batch":{"input_config":{"gcs_source":"x"}}}`,
		"file name is not a string":    `{"batch":{"input_config":{"file_name":1}}}`,
		"file of the provider":         `{"batch":{"input_config":{"file_name":"files/abc123"}}}`,
		"file without the prefix":      `{"batch":{"input_config":{"file_name":"` + strings.TrimPrefix(file, "files/") + `"}}}`,
		"requests is not wrapped":      `{"batch":{"input_config":{"requests":[` + item("a") + `]}}}`,
		"no requests":                  inline(),
		"item with an extra field":     inline(`{"request":` + generate + `,"metadata":{"key":"a"},"priority":1}`),
		"item without metadata":        inline(`{"request":` + generate + `}`),
		"metadata with an extra field": inline(`{"request":` + generate + `,"metadata":{"key":"a","tag":"b"}}`),
		"empty key":                    inline(item("")),
		"key is too long":              inline(item(strings.Repeat("k", 129))),
		"repeated key":                 inline(item("a"), item("a")),
		"request is not an object":     inline(`{"request":"x","metadata":{"key":"a"}}`),
		"request is not a generation":  inline(`{"request":{"contents":"x"},"metadata":{"key":"a"}}`),
		"request names a file":         inline(`{"request":{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"files/abc"}}]}]},"metadata":{"key":"a"}}`),
	} {
		if got, err := validateBatch([]byte(body), "route"); err == nil || err.Status != 400 {
			t.Errorf("%s: accepted as %+v with %+v, want a 400", name, got, err)
		}
	}
}
