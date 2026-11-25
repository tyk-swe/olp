package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type A2AVersion string

const (
	A2AVersion03 A2AVersion = "0.3"
	A2AVersion10 A2AVersion = "1.0"
)

func WithA2AVersion(version A2AVersion) RequestEditorFn {
	return func(_ context.Context, request *http.Request) error {
		request.Header.Set("A2A-Version", string(version))
		return nil
	}
}

type A2ANumericID json.Number

func (id A2ANumericID) MarshalJSON() ([]byte, error) {
	return json.Marshal(json.Number(id))
}

func (id *A2ANumericID) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return errors.New("A2A numeric ID must be a JSON number")
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*id = A2ANumericID(number)
	return nil
}
