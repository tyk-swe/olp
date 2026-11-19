package realtimeresponses

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
)

// parseObject validates the entire native frame, including unknown nested
// controls, without materializing a second JSON tree for the few observed
// response fields. OIF rejects duplicate keys and invalid Unicode throughout.
func parseObject(data []byte) (oif.Value, bool) {
	doc, err := ingress.ParseObjectRequest(data, oif.Limits{MaxBytes: maxFrameBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Value{}, false
	}
	return doc.Root(), true
}

// flatFrame recognizes the common native audio/event shape without
// building an index for fields the relay never interprets. It accepts only a
// flat object with unescaped keys and string values; every other shape falls
// through to OIF. The fixed key table catches duplicate members, while
// json.Valid and UTF-8 validation cover the complete flat document. Escapes
// fall through so OIF still enforces surrogate-pair and decoded-key rules.
func flatFrame(data []byte) (frame, bool) {
	var event frame
	if len(data) > maxFrameBytes || !utf8.Valid(data) {
		return event, false
	}
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
		return event, false
	}
	var keys [16][]byte
	count, at := 0, 1
	space := func() {
		for at < len(data) && (data[at] == ' ' || data[at] == '\t' || data[at] == '\r' || data[at] == '\n') {
			at++
		}
	}
	for {
		space()
		if at >= len(data) {
			return frame{}, false
		}
		if data[at] == '}' {
			at++
			break
		}
		if data[at] != '"' || count == len(keys) {
			return frame{}, false
		}
		at++
		start := at
		for at < len(data) && data[at] != '"' {
			if data[at] == '\\' {
				return frame{}, false
			}
			at++
		}
		if at >= len(data) {
			return frame{}, false
		}
		key := data[start:at]
		// Case-insensitive observed fields need the full decoder, including
		// its strict ambiguity checks. Response is structured even if a caller
		// sends a scalar value.
		if bytes.EqualFold(key, []byte("response")) ||
			bytes.EqualFold(key, []byte("type")) && !bytes.Equal(key, []byte("type")) ||
			bytes.EqualFold(key, []byte("response_id")) && !bytes.Equal(key, []byte("response_id")) {
			return frame{}, false
		}
		for _, prior := range keys[:count] {
			if bytes.Equal(prior, key) {
				return frame{}, false
			}
		}
		keys[count], count = key, count+1
		at++
		space()
		if at >= len(data) || data[at] != ':' {
			return frame{}, false
		}
		at++
		space()
		if at >= len(data) || data[at] == '{' || data[at] == '[' {
			return frame{}, false
		}
		quoted := data[at] == '"'
		if quoted {
			at++
		}
		start = at
		if quoted {
			for at < len(data) && data[at] != '"' {
				if data[at] == '\\' {
					return frame{}, false
				}
				at++
			}
		} else {
			for at < len(data) && data[at] != ',' && data[at] != '}' {
				at++
			}
		}
		if at >= len(data) {
			return frame{}, false
		}
		value := data[start:at]
		if quoted {
			at++
		}
		switch {
		case bytes.Equal(key, []byte("type")):
			if !quoted {
				return frame{}, false
			}
			event.Type = string(value)
		case bytes.Equal(key, []byte("response_id")):
			if !quoted {
				return frame{}, false
			}
			event.ResponseID = string(value)
		}
		space()
		if at >= len(data) {
			return frame{}, false
		}
		if data[at] == '}' {
			at++
			break
		}
		if data[at] != ',' {
			return frame{}, false
		}
		at++
	}
	if at != len(data) || !json.Valid(data) {
		return frame{}, false
	}
	return event, true
}

func member(object oif.Value, name string) (oif.Value, bool) {
	var value oif.Value
	found := false
	for _, member := range object.Members() {
		if strings.EqualFold(member.Name, name) {
			if found {
				// An alias must not hide an earlier malformed accounting field.
				return oif.Value{}, true
			}
			value, found = member.Value, true
		}
	}
	return value, found
}

func stringField(object oif.Value, name string) (string, bool) {
	value, present := member(object, name)
	if !present || value.Kind() == oif.Null {
		return "", true
	}
	if value.Kind() != oif.String {
		return "", false
	}
	raw := value.Raw()
	if !strings.ContainsRune(raw, '\\') {
		return raw[1 : len(raw)-1], true
	}
	// Escaped strings are uncommon on the hot path. JSON, rather than Go's
	// Unquote, keeps surrogate-pair interpretation identical to the old decode.
	var decoded string
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return "", false
	}
	return decoded, true
}

func intField(object oif.Value, name string) (int64, bool) {
	value, present := member(object, name)
	if !present || value.Kind() == oif.Null {
		return 0, true
	}
	if value.Kind() != oif.Number {
		return 0, false
	}
	number, err := strconv.ParseInt(value.Raw(), 10, 64)
	return number, err == nil
}

// responseUsage is the usage object of a terminal realtime response,
// including the modality shares a multimodal session reports.
type responseUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens  *int64 `json:"cached_tokens"`
		AudioTokens   *int64 `json:"audio_tokens"`
		ImageTokens   *int64 `json:"image_tokens"`
		CachedDetails *struct {
			AudioTokens *int64 `json:"audio_tokens"`
			ImageTokens *int64 `json:"image_tokens"`
		} `json:"cached_tokens_details"`
	} `json:"input_token_details"`
	OutputDetails *struct {
		AudioTokens *int64 `json:"audio_tokens"`
	} `json:"output_token_details"`
}

type frame struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id"`
	Response   *struct {
		ID    string         `json:"id"`
		Usage *responseUsage `json:"usage"`
	} `json:"response"`
}

// decodeStrict extracts only the fields used by terminal and
// usage accounting from one duplicate-safe OIF parse. Unknown members remain
// byte-identical in the forwarded frame and are still fully syntax checked.
func decodeStrict(data []byte, event *frame) bool {
	if flat, ok := flatFrame(data); ok {
		*event = flat
		return true
	}
	root, valid := parseObject(data)
	if !valid {
		return false
	}
	if event.Type, valid = stringField(root, "type"); !valid {
		return false
	}
	if event.ResponseID, valid = stringField(root, "response_id"); !valid {
		return false
	}
	response, present := member(root, "response")
	if !present || response.Kind() == oif.Null {
		return true
	}
	if response.Kind() != oif.Object {
		return false
	}
	event.Response = new(struct {
		ID    string         `json:"id"`
		Usage *responseUsage `json:"usage"`
	})
	if event.Response.ID, valid = stringField(response, "id"); !valid {
		return false
	}
	usageValue, present := member(response, "usage")
	if !present || usageValue.Kind() == oif.Null {
		return true
	}
	if usageValue.Kind() != oif.Object {
		return false
	}
	event.Response.Usage = new(responseUsage)
	counter := func(name string) (*int64, bool) {
		value, present := member(usageValue, name)
		if !present || value.Kind() == oif.Null {
			return nil, true
		}
		count, valid := intField(usageValue, name)
		if !valid {
			return nil, false
		}
		return &count, true
	}
	if event.Response.Usage.InputTokens, valid = counter("input_tokens"); !valid {
		return false
	}
	if event.Response.Usage.OutputTokens, valid = counter("output_tokens"); !valid {
		return false
	}
	details, present := member(usageValue, "input_token_details")
	if !present || details.Kind() == oif.Null {
		return true
	}
	if details.Kind() != oif.Object {
		return false
	}
	event.Response.Usage.InputDetails = new(struct {
		CachedTokens  *int64 `json:"cached_tokens"`
		AudioTokens   *int64 `json:"audio_tokens"`
		ImageTokens   *int64 `json:"image_tokens"`
		CachedDetails *struct {
			AudioTokens *int64 `json:"audio_tokens"`
			ImageTokens *int64 `json:"image_tokens"`
		} `json:"cached_tokens_details"`
	})
	readDetail := func(parent oif.Value, name string) (*int64, bool) {
		value, present := member(parent, name)
		if !present || value.Kind() == oif.Null {
			return nil, true
		}
		count, valid := intField(parent, name)
		if !valid {
			return nil, false
		}
		return &count, true
	}
	var ok bool
	if event.Response.Usage.InputDetails.CachedTokens, ok = readDetail(details, "cached_tokens"); !ok {
		return false
	}
	if event.Response.Usage.InputDetails.AudioTokens, ok = readDetail(details, "audio_tokens"); !ok {
		return false
	}
	if event.Response.Usage.InputDetails.ImageTokens, ok = readDetail(details, "image_tokens"); !ok {
		return false
	}
	if cachedDetails, present := member(details, "cached_tokens_details"); present && cachedDetails.Kind() != oif.Null {
		if cachedDetails.Kind() != oif.Object {
			return false
		}
		event.Response.Usage.InputDetails.CachedDetails = new(struct {
			AudioTokens *int64 `json:"audio_tokens"`
			ImageTokens *int64 `json:"image_tokens"`
		})
		if event.Response.Usage.InputDetails.CachedDetails.AudioTokens, ok = readDetail(cachedDetails, "audio_tokens"); !ok {
			return false
		}
		if event.Response.Usage.InputDetails.CachedDetails.ImageTokens, ok = readDetail(cachedDetails, "image_tokens"); !ok {
			return false
		}
	}
	if outputDetails, present := member(usageValue, "output_token_details"); present && outputDetails.Kind() != oif.Null {
		if outputDetails.Kind() != oif.Object {
			return false
		}
		event.Response.Usage.OutputDetails = new(struct {
			AudioTokens *int64 `json:"audio_tokens"`
		})
		if event.Response.Usage.OutputDetails.AudioTokens, ok = readDetail(outputDetails, "audio_tokens"); !ok {
			return false
		}
	}
	return true
}
