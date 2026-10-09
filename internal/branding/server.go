// Package branding manages the installation's authenticated console identity.
package branding

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/access"
)

const maxLogoBytes = 64 << 10

type Identity struct {
	Name string `json:"name"`
	Logo string `json:"logo"`
	ETag string `json:"etag"`
}

type Server struct{ Access *access.Server }

func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/branding", s.get)
	s.Access.Route(mux, "PUT /api/v1/branding", s.update, access.MaxBody(100<<10))
}

func (s *Server) get(r *http.Request, _ access.Principal) (access.Reply, error) {
	var identity Identity
	err := s.Access.Pool.QueryRow(r.Context(), "SELECT name,logo,branding_etag::text FROM olp.installation WHERE singleton").Scan(&identity.Name, &identity.Logo, &identity.ETag)
	return access.Detail(identity, identity.ETag), err
}

func validateLogo(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	mime, encoded, ok := strings.Cut(raw, ";base64,")
	if !ok || (mime != "data:image/png" && mime != "data:image/jpeg") || len(encoded) > base64.StdEncoding.EncodedLen(maxLogoBytes) {
		return "", access.Invalid("logo", "Use a PNG or JPEG logo of at most 64 KiB.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > maxLogoBytes {
		return "", access.Invalid("logo", "Use a valid base64-encoded PNG or JPEG logo.")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || mime != "data:image/"+format || config.Width < 1 || config.Height < 1 || config.Width > 512 || config.Height > 512 {
		return "", access.Invalid("logo", "Use a valid PNG or JPEG logo up to 512 × 512 pixels.")
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return "", access.Invalid("logo", "Use a complete PNG or JPEG image.")
	}
	return mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (s *Server) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		Name string  `json:"name"`
		Logo *string `json:"logo"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 100 || strings.ContainsFunc(input.Name, unicode.IsControl) {
		return access.Reply{}, access.Invalid("name", "Use an installation name of 1–100 characters without control characters.")
	}
	if input.Logo == nil {
		return access.Reply{}, access.Invalid("logo", "Include a logo, or an empty string to use the default brand mark.")
	}
	logo, err := validateLogo(*input.Logo)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var identity Identity
	if err = tx.QueryRow(r.Context(), "SELECT name,logo,branding_etag::text FROM olp.installation WHERE singleton FOR UPDATE").Scan(&identity.Name, &identity.Logo, &identity.ETag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, identity.ETag); err != nil {
		return access.Reply{}, err
	}
	identity = Identity{Name: input.Name, Logo: logo, ETag: access.NewID()}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.installation SET name=$1,logo=$2,branding_etag=$3 WHERE singleton", identity.Name, identity.Logo, identity.ETag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "branding.update", "installation", s.Access.Installation, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(identity, identity.ETag))
}
