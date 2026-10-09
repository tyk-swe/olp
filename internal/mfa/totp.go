// Package mfa contains local second-factor cryptographic helpers.
package mfa

import (
	"crypto/subtle"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
)

// Counter returns a fresh RFC 6238 counter within one 30-second step of the
// server clock. A caller atomically persists it to prevent cross-gateway replay.
func Counter(secret, code string, now time.Time, last int64) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	current := now.Unix() / 30
	for counter := current - 1; counter <= current+1; counter++ {
		if counter < 0 || counter <= last {
			continue
		}
		expected, err := hotp.GenerateCodeCustom(secret, uint64(counter), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return counter, true
		}
	}
	return 0, false
}
