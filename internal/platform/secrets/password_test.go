package secrets

import (
	"slices"
	"strings"
	"testing"
)

func replacePart(parts []string, index int, value string) string {
	changed := slices.Clone(parts)
	changed[index] = value
	return strings.Join(changed, "$")
}

func TestPasswordRoundTrip(t *testing.T) {
	first := HashPassword("correct horse")
	second := HashPassword("correct horse")
	if first == second {
		t.Fatal("two hashes of one password are identical, so the salt is not random")
	}
	for _, encoded := range []string{first, second} {
		if !VerifyPassword("correct horse", encoded) {
			t.Fatal("the right password does not verify against its own hash")
		}
		if VerifyPassword("wrong horse", encoded) {
			t.Fatal("a wrong password verifies")
		}
	}
}

func TestVerifyPasswordRejectsMalformedRecords(t *testing.T) {
	valid := HashPassword("correct horse")
	parts := strings.Split(valid, "$")
	records := map[string]string{
		"empty":           "",
		"truncated":       valid[:len(valid)-4],
		"other scheme":    replacePart(parts, 1, "argon2i"),
		"other params":    replacePart(parts, 3, "m=8,t=1,p=1"),
		"garbage salt":    replacePart(parts, 4, "!!!"),
		"garbage digest":  replacePart(parts, 5, "!!!"),
		"short digest":    replacePart(parts, 5, parts[5][:8]),
		"swapped records": HashPassword("other password"),
	}
	for name, record := range records {
		if VerifyPassword("correct horse", record) {
			t.Fatalf("%s record verifies", name)
		}
	}
	if VerifyPassword(strings.Repeat("x", 4097), valid) {
		t.Fatal("an overlong password verifies")
	}
}
