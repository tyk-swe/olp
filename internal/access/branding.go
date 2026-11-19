package access

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

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

const maxLogoBytes = 64 << 10

type brandingIdentity struct {
	Name        string       `json:"name"`
	Logo        string       `json:"logo"`
	Accent      string       `json:"accent"`
	AccentScale *accentScale `json:"accent_scale"`
	ETag        string       `json:"etag"`
}

func newBrandingIdentity(name, logo, accent, etag string) brandingIdentity {
	return brandingIdentity{Name: name, Logo: logo, Accent: accent, AccentScale: accentScaleOf(accent), ETag: etag}
}

func accentScaleOf(accent string) *accentScale {
	scale, err := deriveAccent(accent)
	if err != nil {
		return nil
	}
	return &scale
}

func (s *Server) branding(r *http.Request) (httpkit.Reply, error) {
	var name, logo, accent, etag string
	err := s.Pool.QueryRow(r.Context(), "SELECT name,logo,accent,branding_etag::text FROM olp.installation WHERE singleton").Scan(&name, &logo, &accent, &etag)
	return httpkit.Detail(newBrandingIdentity(name, logo, accent, etag), etag), err
}

func validateLogo(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	mime, encoded, ok := strings.Cut(raw, ";base64,")
	if !ok || (mime != "data:image/png" && mime != "data:image/jpeg") || len(encoded) > base64.StdEncoding.EncodedLen(maxLogoBytes) {
		return "", httpkit.Invalid("logo", "Use a PNG or JPEG logo of at most 64 KiB.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > maxLogoBytes {
		return "", httpkit.Invalid("logo", "Use a valid base64-encoded PNG or JPEG logo.")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || mime != "data:image/"+format || config.Width < 1 || config.Height < 1 || config.Width > 512 || config.Height > 512 {
		return "", httpkit.Invalid("logo", "Use a valid PNG or JPEG logo up to 512 × 512 pixels.")
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return "", httpkit.Invalid("logo", "Use a complete PNG or JPEG image.")
	}
	return mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (s *Server) updateBranding(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Name   string  `json:"name"`
		Logo   *string `json:"logo"`
		Accent *string `json:"accent"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 100 || strings.ContainsFunc(input.Name, unicode.IsControl) {
		return httpkit.Reply{}, httpkit.Invalid("name", "Use an installation name of 1–100 characters without control characters.")
	}
	if input.Logo == nil {
		return httpkit.Reply{}, httpkit.Invalid("logo", "Include a logo, or an empty string to use the default brand mark.")
	}
	logo, err := validateLogo(*input.Logo)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if input.Accent == nil {
		return httpkit.Reply{}, httpkit.Invalid("accent", "Include a brand color, or an empty string to use the default accent.")
	}
	accent, err := normalizeAccent(*input.Accent)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return Transact(s, r, func(tx pgx.Tx, _ authz.Principal) (struct{}, error) {
		var current string
		if err := tx.QueryRow(r.Context(), "SELECT branding_etag::text FROM olp.installation WHERE singleton FOR UPDATE").Scan(&current); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, httpkit.Match(r, current)
	}, func(tx pgx.Tx, _ authz.Principal, _ struct{}) (httpkit.Reply, Event, error) {
		identity := newBrandingIdentity(input.Name, logo, accent, httpkit.NewID())
		_, err := tx.Exec(r.Context(), "UPDATE olp.installation SET name=$1,logo=$2,accent=$3,branding_etag=$4 WHERE singleton", identity.Name, identity.Logo, identity.Accent, identity.ETag)
		return httpkit.Detail(identity, identity.ETag), Event{Action: "branding.update", Resource: "installation", ID: s.Installation}, err
	})
}
