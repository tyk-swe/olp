package identity

import "regexp"

var (
	digest     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	executable = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func ValidDigest(value string) bool {
	return digest.MatchString(value)
}

func ValidExecutable(value string) bool {
	return executable.MatchString(value)
}
