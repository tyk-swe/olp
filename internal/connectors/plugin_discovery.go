package connectors

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// listingPath is a path of one or more segments under the address.
var listingPath = regexp.MustCompile(`^(/[A-Za-z0-9._~!$'()*+,;=:@-]+)+$`)

// validateDiscovery checks a hosting adaptation's model listing. Its cursor
// parameter joins the address's query, so it must not be one the profile
// places.
func validateDiscovery(declared abi.Hosting) error {
	discovery := declared.Discovery
	if discovery == nil {
		return nil
	}
	if len(discovery.Path) > 512 || !listingPath.MatchString(discovery.Path) || slices.ContainsFunc(strings.Split(discovery.Path, "/"), func(segment string) bool { return segment == "." || segment == ".." }) {
		return &ProfileError{Field: "hosting.discovery.path", Message: "Declare the listing's path under the address, such as /models: segments of letters, digits and URL punctuation, without dot segments, query or fragment."}
	}
	fields := []struct{ field, name string }{{"hosting.discovery.models", discovery.Models}, {"hosting.discovery.id", discovery.ID}}
	if pagination := discovery.Pagination; pagination != nil {
		field := "hosting.discovery.pagination.parameter"
		_, placed := declared.Query[pagination.Parameter]
		switch {
		case !queryName.MatchString(pagination.Parameter):
			return &ProfileError{Field: field, Message: "Name the query parameter with 1–128 letters, digits, dots, underscores, tildes and hyphens."}
		case placed:
			return &ProfileError{Field: field, Message: "The hosting adaptation already places this query parameter."}
		}
		fields = append(fields, struct{ field, name string }{"hosting.discovery.pagination.cursor", pagination.Cursor})
		if pagination.More != "" {
			fields = append(fields, struct{ field, name string }{"hosting.discovery.pagination.more", pagination.More})
		}
	}
	for _, f := range fields {
		if f.name == "" || utf8.RuneCountInString(f.name) > 128 || !utf8.ValidString(f.name) || strings.ContainsFunc(f.name, unicode.IsControl) {
			return &ProfileError{Field: f.field, Message: "Name a top-level field of the listing with 1–128 characters, without control characters."}
		}
	}
	return nil
}

// Discovery returns the model listing the profile declares, if it declares
// one.
func (p *PluginProfile) Discovery() (abi.Discovery, bool) {
	declared := p.declared.Hosting.Discovery
	if declared == nil {
		return abi.Discovery{}, false
	}
	discovery := *declared
	if declared.Pagination != nil {
		pagination := *declared.Pagination
		discovery.Pagination = &pagination
	}
	return discovery, true
}
