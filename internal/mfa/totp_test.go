package mfa

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
)

func TestTOTPCountersRejectReplayAndBoundClockSkew(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	now := time.Unix(1770000000, 0)
	counter := now.Unix() / 30
	for _, delta := range []int64{-2, -1, 0, 1, 2} {
		code, err := hotp.GenerateCodeCustom(secret, uint64(counter+delta), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := Counter(secret, code, now, -1)
		want := delta >= -1 && delta <= 1
		if ok != want || ok && got != counter+delta {
			t.Fatalf("delta=%d counter=%d accepted=%v", delta, got, ok)
		}
		if ok {
			if _, replayed := Counter(secret, code, now, got); replayed {
				t.Fatal("code replayed")
			}
		}
	}
	code, err := hotp.GenerateCodeCustom(secret, uint64(counter), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	next := now.Add(30 * time.Second)
	if got, ok := Counter(secret, code, next, counter-1); !ok || got != counter {
		t.Fatal("previous-step code should remain valid within clock skew")
	}
	if _, replayed := Counter(secret, code, next, counter); replayed {
		t.Fatal("consumed counter replayed across a time-step boundary")
	}
	nextCode, err := hotp.GenerateCodeCustom(secret, uint64(counter+1), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := Counter(secret, nextCode, next, counter); !ok || got != counter+1 {
		t.Fatal("fresh next-step code should remain valid")
	}
	for _, code := range []string{"", "12345", "1234567", "12 456", "１２３４５６"} {
		if _, ok := Counter(secret, code, now, -1); ok {
			t.Fatal("malformed code accepted")
		}
	}
}
