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
	if len(data) > maxFrameBytes || !utf8.Valid(data) {
		return frame{}, false
	}
	scan := flatScanner{data: bytes.TrimSpace(data), at: 1}
	if len(scan.data) < 2 || scan.data[0] != '{' || scan.data[len(scan.data)-1] != '}' {
		return frame{}, false
	}
	var event frame
	var keys [16][]byte
	count := 0
	for {
		next, more := scan.peek()
		if !more {
			return frame{}, false
		}
		if next == '}' {
			scan.at++
			break
		}
		if next != '"' || count == len(keys) {
			return frame{}, false
		}
		key, value, quoted, ok := scan.member()
		if !ok || needsFullDecoder(key) || contains(keys[:count], key) {
			return frame{}, false
		}
		keys[count], count = key, count+1
		observed := bytes.Equal(key, []byte("type")) || bytes.Equal(key, []byte("response_id"))
		if observed && !quoted {
			return frame{}, false
		}
		switch {
		case bytes.Equal(key, []byte("type")):
			event.Type = string(value)
		case bytes.Equal(key, []byte("response_id")):
			event.ResponseID = string(value)
		}
		next, more = scan.peek()
		if !more {
			return frame{}, false
		}
		if next == '}' {
			scan.at++
			break
		}
		if next != ',' {
			return frame{}, false
		}
		scan.at++
	}
	if scan.at != len(scan.data) || !json.Valid(scan.data) {
		return frame{}, false
	}
	return event, true
}

// Case-insensitive observed fields need the full decoder, including its
// strict ambiguity checks. Response is structured even if a caller sends a
// scalar value.
func needsFullDecoder(key []byte) bool {
	return bytes.EqualFold(key, []byte("response")) ||
		bytes.EqualFold(key, []byte("type")) && !bytes.Equal(key, []byte("type")) ||
		bytes.EqualFold(key, []byte("response_id")) && !bytes.Equal(key, []byte("response_id"))
}

func contains(keys [][]byte, key []byte) bool {
	for _, prior := range keys {
		if bytes.Equal(prior, key) {
			return true
		}
	}
	return false
}

type flatScanner struct {
	data []byte
	at   int
}

func (s *flatScanner) peek() (byte, bool) {
	for s.at < len(s.data) && (s.data[s.at] == ' ' || s.data[s.at] == '\t' || s.data[s.at] == '\r' || s.data[s.at] == '\n') {
		s.at++
	}
	if s.at >= len(s.data) {
		return 0, false
	}
	return s.data[s.at], true
}

func (s *flatScanner) member() (key, value []byte, quoted, ok bool) {
	key, ok = s.text()
	if !ok {
		return nil, nil, false, false
	}
	if next, more := s.peek(); !more || next != ':' {
		return nil, nil, false, false
	}
	s.at++
	next, more := s.peek()
	if !more || next == '{' || next == '[' {
		return nil, nil, false, false
	}
	if next == '"' {
		value, ok = s.text()
		return key, value, true, ok
	}
	value, ok = s.bare()
	return key, value, false, ok
}

func (s *flatScanner) text() ([]byte, bool) {
	s.at++
	start := s.at
	for s.at < len(s.data) && s.data[s.at] != '"' {
		if s.data[s.at] == '\\' {
			return nil, false
		}
		s.at++
	}
	if s.at >= len(s.data) {
		return nil, false
	}
	s.at++
	return s.data[start : s.at-1], true
}

func (s *flatScanner) bare() ([]byte, bool) {
	start := s.at
	for s.at < len(s.data) && s.data[s.at] != ',' && s.data[s.at] != '}' {
		s.at++
	}
	return s.data[start:s.at], s.at < len(s.data)
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
	InputTokens   *int64         `json:"input_tokens"`
	OutputTokens  *int64         `json:"output_tokens"`
	InputDetails  *inputDetails  `json:"input_token_details"`
	OutputDetails *outputDetails `json:"output_token_details"`
}

type inputDetails struct {
	CachedTokens  *int64         `json:"cached_tokens"`
	AudioTokens   *int64         `json:"audio_tokens"`
	ImageTokens   *int64         `json:"image_tokens"`
	CachedDetails *cachedDetails `json:"cached_tokens_details"`
}

type cachedDetails struct {
	AudioTokens *int64 `json:"audio_tokens"`
	ImageTokens *int64 `json:"image_tokens"`
}

type outputDetails struct {
	AudioTokens *int64 `json:"audio_tokens"`
}

type responseBody struct {
	ID    string         `json:"id"`
	Usage *responseUsage `json:"usage"`
}

type frame struct {
	Type       string        `json:"type"`
	ResponseID string        `json:"response_id"`
	Response   *responseBody `json:"response"`
}

// decodeStrict extracts only the fields used by terminal and
// usage accounting from one duplicate-safe OIF parse. Unknown members remain
// byte-identical in the forwarded frame and are still fully syntax checked.
func decodeStrict(data []byte) (frame, bool) {
	if flat, ok := flatFrame(data); ok {
		return flat, true
	}
	root, valid := parseObject(data)
	if !valid {
		return frame{}, false
	}
	eventType, validType := stringField(root, "type")
	responseID, validID := stringField(root, "response_id")
	response, validResponse := decodeResponse(root)
	return frame{Type: eventType, ResponseID: responseID, Response: response}, validType && validID && validResponse
}

func decodeResponse(root oif.Value) (*responseBody, bool) {
	response, present, valid := optionalObject(root, "response")
	if !present || !valid {
		return nil, valid
	}
	id, validID := stringField(response, "id")
	usage, validUsage := decodeUsage(response)
	return &responseBody{ID: id, Usage: usage}, validID && validUsage
}

func decodeUsage(response oif.Value) (*responseUsage, bool) {
	usage, present, valid := optionalObject(response, "usage")
	if !present || !valid {
		return nil, valid
	}
	input, validInput := optionalCount(usage, "input_tokens")
	output, validOutput := optionalCount(usage, "output_tokens")
	inputShares, validInputShares := decodeInputDetails(usage)
	outputShares, validOutputShares := decodeOutputDetails(usage)
	return &responseUsage{InputTokens: input, OutputTokens: output, InputDetails: inputShares, OutputDetails: outputShares},
		validInput && validOutput && validInputShares && validOutputShares
}

func decodeInputDetails(usage oif.Value) (*inputDetails, bool) {
	details, present, valid := optionalObject(usage, "input_token_details")
	if !present || !valid {
		return nil, valid
	}
	cachedTokens, validCached := optionalCount(details, "cached_tokens")
	audio, validAudio := optionalCount(details, "audio_tokens")
	image, validImage := optionalCount(details, "image_tokens")
	cached, validShares := decodeCachedDetails(details)
	return &inputDetails{CachedTokens: cachedTokens, AudioTokens: audio, ImageTokens: image, CachedDetails: cached},
		validCached && validAudio && validImage && validShares
}

func decodeCachedDetails(details oif.Value) (*cachedDetails, bool) {
	cached, present, valid := optionalObject(details, "cached_tokens_details")
	if !present || !valid {
		return nil, valid
	}
	audio, validAudio := optionalCount(cached, "audio_tokens")
	image, validImage := optionalCount(cached, "image_tokens")
	return &cachedDetails{AudioTokens: audio, ImageTokens: image}, validAudio && validImage
}

func decodeOutputDetails(usage oif.Value) (*outputDetails, bool) {
	details, present, valid := optionalObject(usage, "output_token_details")
	if !present || !valid {
		return nil, valid
	}
	audio, validAudio := optionalCount(details, "audio_tokens")
	return &outputDetails{AudioTokens: audio}, validAudio
}

func optionalObject(parent oif.Value, name string) (value oif.Value, present, valid bool) {
	value, present = member(parent, name)
	if !present || value.Kind() == oif.Null {
		return oif.Value{}, false, true
	}
	return value, true, value.Kind() == oif.Object
}

func optionalCount(parent oif.Value, name string) (*int64, bool) {
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
