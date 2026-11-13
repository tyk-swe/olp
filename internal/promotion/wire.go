package promotion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

func Convert[T any](from any) (T, error) {
	var to T
	data, err := json.Marshal(from)
	if err != nil {
		return to, err
	}
	if err := json.Unmarshal(data, &to); err != nil {
		return to, err
	}
	return to, nil
}

func ConvertUnique[T any](from any) (T, error) {
	var to T
	data, err := json.Marshal(from)
	if err != nil {
		return to, err
	}
	request, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(data))
	if err != nil {
		return to, err
	}
	request.Header.Set("Content-Type", "application/json")
	if err := access.DecodeUnique(request, &to, int64(len(data))); err != nil {
		return to, err
	}
	return to, nil
}

func Digest(document contract.ConfigurationDocument) (string, error) {
	data, err := json.Marshal(struct {
		contract.ConfigurationDocument
		ExportedAt *time.Time `json:"exported_at,omitempty"`
	}{ConfigurationDocument: document})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func Present[T any](value nullable.Nullable[T]) (T, bool) {
	present, err := value.Get()
	return present, err == nil
}

func Pointer[T any](value nullable.Nullable[T]) *T {
	present, ok := Present(value)
	if !ok {
		return nil
	}
	return &present
}

func Nullable[T any](value *T) nullable.Nullable[T] {
	if value == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*value)
}

func TupleKey(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	return string(encoded)
}
