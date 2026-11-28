package textfield

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/domain/problem"
)

func Validate(field, value string, limit int) error {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > limit {
		return problem.Invalid(field, "Use 1–"+strconv.Itoa(limit)+" characters.")
	}
	return nil
}
