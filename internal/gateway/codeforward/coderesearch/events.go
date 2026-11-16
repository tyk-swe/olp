package coderesearch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// Resume IDs authenticate the opaque upstream event ID to the OLP session.
// This also fences stateless upstream streams shared by several OLP keys.
// Derivation from the enrolled bearer is domain-separated and replica-stable;
// neither the bearer nor result content is included in the resulting token.
func SealEventID(session, event, credential string) (string, error) {
	if !validEventID(event) || session == "" || credential == "" {
		return "", ErrProtocol
	}
	return base64.RawURLEncoding.EncodeToString([]byte(event)) + "." + base64.RawURLEncoding.EncodeToString(eventMAC(session, event, credential)), nil
}

func OpenEventID(session, token, credential string) (string, error) {
	if len(token) > 1500 || session == "" || credential == "" {
		return "", ErrProtocol
	}
	encoded, signature, ok := strings.Cut(token, ".")
	event, err := base64.RawURLEncoding.DecodeString(encoded)
	if !ok || err != nil || !validEventID(string(event)) {
		return "", ErrProtocol
	}
	mac, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(mac, eventMAC(session, string(event), credential)) {
		return "", ErrProtocol
	}
	return string(event), nil
}

func eventMAC(session, event, credential string) []byte {
	derive := hmac.New(sha256.New, []byte(credential))
	derive.Write([]byte("olp/research/event-ids/v1"))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write([]byte(session))
	mac.Write([]byte{0})
	mac.Write([]byte(event))
	return mac.Sum(nil)
}

func validEventID(value string) bool {
	return len(value) <= 1024 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
