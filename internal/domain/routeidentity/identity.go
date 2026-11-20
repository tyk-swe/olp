package routeidentity

import "regexp"

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
var selectorID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func ValidSlug(value string) bool {
	return slug.MatchString(value)
}

func ValidSelectorID(value string) bool {
	return selectorID.MatchString(value)
}
