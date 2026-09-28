package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const maxManifestBytes = 64 << 10

var (
	identifier   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	versionLabel = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
)

// decodeManifest reads the manifest a plugin reported, which is unconfined
// or not. Fields this OLP does not know make it invalid rather than ignored: a
// plugin must not declare behaviour that would silently not run.
func decodeManifest(raw []byte, unconfined bool) (abi.Manifest, error) {
	var m abi.Manifest
	if len(raw) > maxManifestBytes {
		return m, invalidManifest("manifest", "The manifest exceeds 64 KiB.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, invalidManifest("manifest", "The manifest is not one this OLP understands: "+err.Error()+".")
	}
	return m, validateManifest(m, unconfined)
}

func validateManifest(m abi.Manifest, unconfined bool) error {
	if !identifier.MatchString(m.Name) {
		return invalidManifest("manifest.name", "Name the plugin with 1–64 lowercase letters, digits and hyphens, starting and ending with a letter or digit.")
	}
	if !versionLabel.MatchString(m.Version) {
		return invalidManifest("manifest.version", "Label the version with 1–64 letters, digits, dots, hyphens, underscores and plus signs.")
	}
	if !displayText(m.Description, 0, 500) {
		return invalidManifest("manifest.description", "Describe the plugin in at most 500 characters, without control characters.")
	}
	if len(m.Origins) > 16 {
		return invalidManifest("manifest.origins", "Declare at most 16 origins.")
	}
	for i, origin := range m.Origins {
		field := fmt.Sprintf("manifest.origins[%d]", i)
		if !canonicalOrigin(origin) {
			return invalidManifest(field, "Declare an http or https origin in canonical form, such as https://api.example.com: lowercase, without a path, credentials or default port.")
		}
		if slices.Contains(m.Origins[:i], origin) {
			return invalidManifest(field, "Declare each origin once.")
		}
	}
	if len(m.Profiles) < 1 || len(m.Profiles) > 16 {
		return invalidManifest("manifest.profiles", "Declare 1–16 profiles.")
	}
	for i, p := range m.Profiles {
		field := fmt.Sprintf("manifest.profiles[%d]", i)
		if !identifier.MatchString(p.ID) {
			return invalidManifest(field+".id", "Identify the profile with 1–64 lowercase letters, digits and hyphens, starting and ending with a letter or digit.")
		}
		if slices.ContainsFunc(m.Profiles[:i], func(prior abi.Profile) bool { return prior.ID == p.ID }) {
			return invalidManifest(field+".id", "Use each profile ID once.")
		}
		if !displayText(p.Label, 1, 100) {
			return invalidManifest(field+".label", "Label the profile with 1–100 characters, without control characters.")
		}
		if p.Dialect == "" {
			return invalidManifest(field+".dialect", "Name the built-in dialect the profile serves.")
		}
		if !slices.Contains(connectors.PluginDialects(), p.Dialect) {
			return &Error{Code: CodeDialectUnknown, Field: field + ".dialect", Message: fmt.Sprintf("Plugin profiles can't serve a dialect named %q. A plugin names one of the built-in dialects %s, whose traffic is HTTP and SSE, and never defines one.", p.Dialect, strings.Join(connectors.PluginDialects(), ", "))}
		}
		if p.CarriesTraffic && !unconfined {
			return invalidManifest(field+".carries_traffic", "Only an unconfined plugin carries traffic. A confined plugin's profiles reach the upstream over OLP's transport.")
		}
		if err := connectors.ValidatePluginProfile(p); err != nil {
			if refusal, ok := errors.AsType[*connectors.ProfileError](err); ok {
				return invalidManifest(field+"."+refusal.Field, refusal.Message)
			}
			return invalidManifest(field, err.Error())
		}
		// An address that begins with a grant fact has no origin of its own:
		// OLP checks each grant's against the origins as it places requests.
		if address, _ := url.Parse(p.Hosting.Address); address.Host != "" && !slices.Contains(m.Origins, address.Scheme+"://"+address.Host) {
			return invalidManifest(field+".hosting.address", "Declare the address at one of the plugin's origins, in the same form.")
		}
	}
	return nil
}

func invalidManifest(field, message string) *Error {
	return &Error{Code: CodeManifestInvalid, Field: field, Message: message}
}

func displayText(text string, least, most int) bool {
	return connectors.PlainText(text, least, most) && strings.TrimSpace(text) == text
}

// canonicalOrigin reports whether origin is an http or https origin written
// exactly as OLP compares origins, so what an owner approves is what applies.
func canonicalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	return origin == connectors.Origin(u)
}
