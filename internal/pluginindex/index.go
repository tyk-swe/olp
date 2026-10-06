// Package pluginindex is the signed index of reviewed provider plugins: for
// each plugin, its source and every reviewed release's module digest, ABI,
// origins and profiles. The console lets owners browse it beside what is
// installed; installing a module, approving its origins and permitting it stay
// explicit owner actions, and the digest of an uploaded module is what ties it
// to an index entry. Load is the only constructor of a verified index.
package pluginindex

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/signing"
)

// APIVersion identifies the plugin index document format.
const APIVersion = "openllmproxy.dev/plugin-index/v1"

// MaxBytes bounds a plugin index document.
const MaxBytes = 1 << 20

// Index is a plugin index document.
type Index struct {
	APIVersion  string    `json:"api_version"`
	PublishedAt time.Time `json:"published_at"`
	Plugins     []Plugin  `json:"plugins"`
}

// Plugin is a reviewed plugin and its source.
type Plugin struct {
	// Name is the name the plugin's manifest declares.
	Name             string `json:"name"`
	Description      string `json:"description"`
	Maintainer       string `json:"maintainer"`
	DocumentationURL string `json:"documentation_url"`
	// Repository holds the plugin's source, at Path within it.
	Repository string    `json:"repository"`
	Path       string    `json:"path"`
	Releases   []Release `json:"releases"`
}

// Release is a reviewed build of a plugin.
type Release struct {
	// Version is the version the plugin's manifest declares.
	Version string `json:"version"`
	// Digest is the hex SHA-256 of the WebAssembly module, as OLP names an
	// installed plugin.
	Digest     string `json:"digest"`
	ABIVersion int    `json:"abi_version"`
	SizeBytes  int64  `json:"size_bytes"`
	// Origins and Profiles are what the module's manifest declares; an owner
	// approves exactly these origins.
	Origins  []string `json:"origins"`
	Profiles []string `json:"profiles"`
	// Commit is the source revision the digest builds from, reproducibly.
	Commit     string    `json:"commit"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

var (
	pluginName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	digestHex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitHex  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	version    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
)

// Decode parses an index document strictly.
func Decode(document []byte) (*Index, error) {
	var index Index
	if err := signing.DecodeDocument(document, "the plugin index", APIVersion, MaxBytes, &index); err != nil {
		return nil, err
	}
	return &index, nil
}

// Validate checks every rule of the format.
func (x *Index) Validate() error {
	if x.APIVersion != APIVersion || x.PublishedAt.IsZero() {
		return errors.New("the plugin index needs its format version and a publication time")
	}
	names, digests := map[string]bool{}, map[string]bool{}
	for _, p := range x.Plugins {
		if !pluginName.MatchString(p.Name) || names[p.Name] || p.Description == "" || p.Maintainer == "" || p.Path == "" || strings.Contains(p.Path, "..") {
			return fmt.Errorf("plugin %q is malformed or repeated", p.Name)
		}
		names[p.Name] = true
		for _, link := range []string{p.DocumentationURL, p.Repository} {
			if u, err := url.Parse(link); err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("plugin %s needs HTTPS documentation and repository URLs", p.Name)
			}
		}
		if len(p.Releases) == 0 {
			return fmt.Errorf("plugin %s has no reviewed release", p.Name)
		}
		for _, r := range p.Releases {
			if !version.MatchString(r.Version) || !digestHex.MatchString(r.Digest) || digests[r.Digest] || r.ABIVersion < 1 || r.SizeBytes < 1 || !commitHex.MatchString(r.Commit) || r.ReviewedAt.IsZero() || r.ReviewedAt.After(x.PublishedAt) {
				return fmt.Errorf("plugin %s release %q is malformed or repeats a digest", p.Name, r.Version)
			}
			digests[r.Digest] = true
			if len(r.Origins) == 0 || len(r.Profiles) == 0 {
				return fmt.Errorf("plugin %s release %s declares no origins or profiles", p.Name, r.Version)
			}
		}
	}
	return nil
}

// Encode renders an index in canonical form: two-space indented JSON with a
// trailing newline, plugins by name, releases newest review first, and every
// list sorted. A signature covers these exact bytes.
func Encode(x *Index) ([]byte, error) {
	normalized := *x
	normalized.PublishedAt = x.PublishedAt.UTC().Truncate(time.Second)
	normalized.Plugins = slices.Clone(x.Plugins)
	for i := range normalized.Plugins {
		p := &normalized.Plugins[i]
		p.Releases = slices.Clone(p.Releases)
		for j := range p.Releases {
			r := &p.Releases[j]
			r.Origins, r.Profiles = sorted(r.Origins), sorted(r.Profiles)
			r.ReviewedAt = r.ReviewedAt.UTC().Truncate(time.Second)
		}
		slices.SortStableFunc(p.Releases, func(a, b Release) int { return b.ReviewedAt.Compare(a.ReviewedAt) })
	}
	slices.SortFunc(normalized.Plugins, func(a, b Plugin) int { return strings.Compare(a.Name, b.Name) })
	return signing.EncodeDocument(normalized)
}

// Canonical decodes, validates and re-encodes an index document.
func Canonical(document []byte) ([]byte, error) {
	x, err := Decode(document)
	if err != nil {
		return nil, err
	}
	if err := x.Validate(); err != nil {
		return nil, err
	}
	return Encode(x)
}

func sorted(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// Signed is an index whose signature verified against a trusted key.
type Signed struct {
	Index  *Index
	SHA256 string
	KeyID  string
}

// Listing is a reviewed release found by its module digest.
type Listing struct {
	Plugin  *Plugin
	Release *Release
}

// Load verifies an index document against its signature with keys, then
// decodes and validates it.
func Load(document, signature []byte, keys signing.Keyring) (*Signed, error) {
	keyID, err := keys.Verify(document, signature)
	if err != nil {
		return nil, fmt.Errorf("plugin index signature: %w", err)
	}
	x, err := Decode(document)
	if err != nil {
		return nil, err
	}
	if err := x.Validate(); err != nil {
		return nil, fmt.Errorf("plugin index: %w", err)
	}
	digest := sha256.Sum256(document)
	return &Signed{Index: x, SHA256: hex.EncodeToString(digest[:]), KeyID: keyID}, nil
}

// Find returns the reviewed release with a module digest.
func (s *Signed) Find(digest string) (Listing, bool) {
	if s == nil {
		return Listing{}, false
	}
	for i := range s.Index.Plugins {
		plugin := &s.Index.Plugins[i]
		for j := range plugin.Releases {
			if plugin.Releases[j].Digest == digest {
				return Listing{Plugin: plugin, Release: &plugin.Releases[j]}, true
			}
		}
	}
	return Listing{}, false
}

var (
	//go:embed index.json
	document []byte
	//go:embed index.json.sig
	signature []byte
)

// Embedded is the index this release ships, verified against the keys this
// build trusts. A process refuses to start when it does not verify.
var Embedded = sync.OnceValues(func() (*Signed, error) {
	return Load(document, signature, signing.Trusted())
})
