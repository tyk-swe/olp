package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var errTrailingData = errors.New("trailing data after the JSON document")

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errTrailingData
	}
	return nil
}
