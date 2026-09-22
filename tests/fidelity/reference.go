// Package fidelity compares externally observable protocol documents without
// importing production codecs. Array order, numeric precision, null, absence,
// empty values and opaque strings remain distinct.
package fidelity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decode rejects duplicate keys rather than hiding corruption behind the
// standard library's last-value-wins object decoding.
func Decode(data []byte) (any, error) {
	if err := validStringEncoding(data); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	value, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON document")
	}
	return value, nil
}

// encoding/json repairs malformed UTF-8 and unpaired UTF-16 surrogate escapes.
// A reference oracle must reject that repair: distinct opaque inputs cannot
// become equal because both were changed to the replacement character.
func validStringEncoding(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			break
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return fmt.Errorf("incomplete Unicode escape")
		}
		code, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return fmt.Errorf("invalid Unicode escape")
		}
		if code >= 0xDC00 && code <= 0xDFFF {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		if code >= 0xD800 && code <= 0xDBFF {
			if i+10 >= len(data) || data[i+5] != '\\' || data[i+6] != 'u' {
				return fmt.Errorf("unpaired Unicode surrogate")
			}
			low, err := strconv.ParseUint(string(data[i+7:i+11]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return fmt.Errorf("unpaired Unicode surrogate")
			}
			i += 10
		} else {
			i += 4
		}
	}
	return nil
}

func decodeValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid object key")
			}
			if _, exists := object[name]; exists {
				return nil, fmt.Errorf("duplicate object key")
			}
			value, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		_, err := d.Token()
		return object, err
	case '[':
		array := []any{}
		for d.More() {
			value, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err := d.Token()
		return array, err
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter")
	}
}

// Compare reports the first differing JSON pointer without exposing values.
// Only insignificant whitespace and object member ordering are ignored.
func Compare(expected, observed []byte) error {
	want, err := Decode(expected)
	if err != nil {
		return fmt.Errorf("invalid reference: %w", err)
	}
	got, err := Decode(observed)
	if err != nil {
		return fmt.Errorf("invalid observation: %w", err)
	}
	return compare(want, got, "")
}

func pointer(path, name string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
}

func compare(want, got any, path string) error {
	mismatch := func(kind string) error { return fmt.Errorf("%s differs at %q", kind, path) }
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return mismatch("object type")
		}
		keys := make([]string, 0, len(w)+len(g))
		for k := range w {
			keys = append(keys, k)
		}
		for k := range g {
			if _, exists := w[k]; !exists {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			wv, wp := w[k]
			gv, gp := g[k]
			if wp != gp {
				return fmt.Errorf("presence differs at %q", pointer(path, k))
			}
			if err := compare(wv, gv, pointer(path, k)); err != nil {
				return err
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return mismatch("array length or type")
		}
		for i := range w {
			if err := compare(w[i], g[i], fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
	default:
		if !reflect.DeepEqual(want, got) {
			return mismatch("value or type")
		}
	}
	return nil
}

type event struct {
	Name string `json:"event"`
	ID   string `json:"id"`
	Data any    `json:"data"`
}

// CompareEvents compares independent SSE observations in delivery order,
// including late state and terminal events. Comments and transport chunk
// boundaries carry no protocol meaning in these fixture contracts.
func CompareEvents(expected, observed []byte) error {
	want, err := events(expected)
	if err != nil {
		return fmt.Errorf("invalid reference stream: %w", err)
	}
	got, err := events(observed)
	if err != nil {
		return fmt.Errorf("invalid observed stream: %w", err)
	}
	w, _ := json.Marshal(want)
	g, _ := json.Marshal(got)
	return Compare(w, g)
}

func events(data []byte) ([]event, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	out := []event{}
	current := event{}
	lastID := ""
	var payload []string
	flush := func() error {
		if len(payload) == 0 {
			current = event{}
			return nil
		}
		joined := strings.Join(payload, "\n")
		if joined == "[DONE]" {
			current.Data = joined
		} else {
			value, err := Decode([]byte(joined))
			if err != nil {
				return err
			}
			current.Data = value
		}
		current.ID = lastID
		out = append(out, current)
		current, payload = event{}, nil
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			current.Name = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				lastID = value
			}
		case "data":
			payload = append(payload, value)
		default:
			return nil, fmt.Errorf("unrecognized fixture SSE field")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		return nil, fmt.Errorf("unterminated SSE frame")
	}
	return out, nil
}
