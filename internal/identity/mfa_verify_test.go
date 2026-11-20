package identity

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
)

func TestTOTPCounterWindowAndReplay(t *testing.T) {
	t.Parallel()
	const secret = "JBSWY3DPEHPK3PXP"
	const step = int64(2000)
	now := time.Unix(step*30+10, 0)
	codeAt := func(counter int64) string {
		code, err := hotp.GenerateCodeCustom(secret, uint64(counter), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if counter, ok := totpCounter(secret, codeAt(step), now, -1); !ok || counter != step {
		t.Fatalf("current step: counter=%d ok=%v", counter, ok)
	}
	if counter, ok := totpCounter(secret, codeAt(step-1), now, -1); !ok || counter != step-1 {
		t.Fatalf("previous step: counter=%d ok=%v", counter, ok)
	}
	if counter, ok := totpCounter(secret, codeAt(step+1), now, -1); !ok || counter != step+1 {
		t.Fatalf("next step: counter=%d ok=%v", counter, ok)
	}
	if _, ok := totpCounter(secret, codeAt(step+2), now, -1); ok {
		t.Fatal("code two steps ahead was accepted")
	}
	if _, ok := totpCounter(secret, codeAt(step), now, step); ok {
		t.Fatal("replayed counter was accepted")
	}
	if _, ok := totpCounter(secret, "12x456", now, -1); ok {
		t.Fatal("non-digit code was accepted")
	}
}
