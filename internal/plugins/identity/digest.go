package identity

import "regexp"

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidDigest(value string) bool {
	return digest.MatchString(value)
}
