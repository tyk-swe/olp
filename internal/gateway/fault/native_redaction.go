package fault

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
)

// A provider error can echo authentication with arbitrary JSON escape
// spelling. Redact decoded strings while OIF keeps every unrelated member and
// number byte-for-byte. A credential-bearing member name cannot be renamed
// without changing the native grammar, so refuse it.
func RedactNativeFailureDocument(doc oif.Document, credentials egress.Sensitive) (oif.Document, error) {
	changes := []oif.Change{}
	var inspect func(oif.Value, string) error
	inspect = func(value oif.Value, pointer string) error {
		switch value.Kind() {
		case oif.Object:
			for _, member := range value.Members() {
				if credentials.Contains(member.Name) {
					return errors.New("failed response event has a credential-bearing member name")
				}
				if err := inspect(member.Value, oif.Pointer(pointer, member.Name)); err != nil {
					return err
				}
			}
		case oif.Array:
			for index, item := range value.Elements() {
				if err := inspect(item, oif.Pointer(pointer, strconv.Itoa(index))); err != nil {
					return err
				}
			}
		case oif.String:
			text, ok := value.Text()
			if !ok {
				return errors.New("failed response event has invalid text")
			}
			if safe := credentials.Redact(text); safe != text {
				encoded, _ := json.Marshal(safe)
				changes = append(changes, oif.Change{Pointer: pointer, Value: string(encoded), Origin: oif.ExplicitTransform, Reason: "provider error credential redaction"})
			}
		}
		return nil
	}
	if err := inspect(doc.Root(), ""); err != nil {
		return oif.Document{}, err
	}
	if len(changes) == 0 {
		return doc, nil
	}
	return oif.Apply(doc, changes)
}
