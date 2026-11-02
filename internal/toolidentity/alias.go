package toolidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var aliasPattern = regexp.MustCompile(`^t_[a-f0-9]{32}_[a-f0-9]{32}$`)

func Alias(server, name string) string {
	digest := sha256.Sum256([]byte(name))
	return "t_" + strings.ReplaceAll(server, "-", "") + "_" + hex.EncodeToString(digest[:16])
}

func ValidAlias(value string) bool {
	return aliasPattern.MatchString(value)
}
