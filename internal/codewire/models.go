package codewire

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func RewriteRequestModel(body []byte, from, to string) ([]byte, error) {
	return rewriteModel(body, from, to, []string{"/model"})
}

func RewriteResponseModel(body []byte, from, to string) ([]byte, error) {
	return rewriteModel(body, from, to, []string{"/model", "/response/model", "/message/model"})
}

func rewriteModel(body []byte, from, to string, pointers []string) ([]byte, error) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return nil, codemode.Refuse(400, "code_body_invalid")
	}
	encoded, _ := json.Marshal(to)
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		for _, pointer := range pointers {
			value, ok := document.Lookup(pointer)
			text, stringValue := value.Text()
			if ok && stringValue && text == from && !yield(oif.Change{Pointer: pointer, Value: string(encoded), Origin: oif.ExplicitTransform, Reason: "Published exact model alias"}) {
				return
			}
		}
	})
	rewritten, err := oif.Apply(document, changes)
	if err != nil {
		return nil, codemode.Refuse(400, "code_body_invalid")
	}
	return rewritten.Bytes(), nil
}
