package workload

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/tyk-swe/olp/internal/oif"
)

type Token struct {
	raw                        string
	issuer, kid, algorithm     string
	expires, issued, notBefore int64
}
type Identity struct {
	Subject   string
	Mapping   Mapping
	EndUser   string
	ExpiresAt time.Time
}

// Parse locates a declared issuer only. No claim is trusted until Verify.
func Parse(raw string) (Token, error) {
	// Every other bearer secret reaches here too, so reject one that is not
	// shaped like a JWT before allocating anything.
	if len(raw) > MaxTokenBytes || strings.Count(raw, ".") != 2 {
		return Token{}, ErrToken
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Token{}, ErrToken
	}
	header, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil || len(header) > 4096 {
		return Token{}, ErrToken
	}
	h, e := oif.ParseJSON(header, oif.Limits{MaxBytes: 4096, MaxDepth: 8, MaxNodes: 64})
	if e != nil {
		return Token{}, ErrToken
	}
	kid, ok := stringClaim(h, "/kid")
	if !ok || len(kid) > 128 {
		return Token{}, ErrToken
	}
	alg, ok := stringClaim(h, "/alg")
	if !ok {
		return Token{}, ErrToken
	}
	d, e := claims(parts[1])
	if e != nil {
		return Token{}, ErrToken
	}
	iss, ok := stringClaim(d, "/iss")
	if !ok || len(iss) > 2048 {
		return Token{}, ErrToken
	}
	exp, valid := dateClaim(d, "/exp")
	iat, issued := dateClaim(d, "/iat")
	if !valid || !issued {
		return Token{}, ErrToken
	}
	var nbf int64
	if _, present := d.Lookup("/nbf"); present {
		var ok bool
		nbf, ok = dateClaim(d, "/nbf")
		if !ok {
			return Token{}, ErrToken
		}
	}
	return Token{raw: raw, issuer: iss, kid: kid, algorithm: alg, expires: exp, issued: iat, notBefore: nbf}, nil
}
func (t Token) Issuer() string { return t.issuer }
func claims(encoded string) (oif.Document, error) {
	data, e := base64.RawURLEncoding.DecodeString(encoded)
	if e != nil {
		return oif.Document{}, ErrToken
	}
	return oif.ParseJSON(data, oif.Limits{MaxBytes: MaxTokenBytes, MaxDepth: 32, MaxNodes: 4096})
}
func stringClaim(d oif.Document, p string) (string, bool) {
	v, ok := d.Lookup(p)
	if !ok {
		return "", false
	}
	s, ok := v.Text()
	return s, ok && s != ""
}
func dateClaim(d oif.Document, p string) (int64, bool) {
	v, ok := d.Lookup(p)
	if !ok || v.Kind() != oif.Number {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(v.Bytes(), &n) != nil {
		return 0, false
	}
	sec, e := strconv.ParseInt(n.String(), 10, 64)
	return sec, e == nil && sec > 0 && sec < 1<<53
}

// Verify validates the signature and every security claim on every request.
func (t Token) Verify(c Config, keys map[string]jose.JSONWebKey, now time.Time) (Identity, error) {
	if !c.Enabled || t.issuer != c.Issuer || !slices.Contains(c.Algorithms, t.algorithm) || slices.Contains(c.DisabledKeyIDs, t.kid) {
		return Identity{}, ErrToken
	}
	key, ok := keys[t.kid]
	if !ok || key.Algorithm != "" && key.Algorithm != t.algorithm {
		return Identity{}, ErrToken
	}
	signature, e := jose.ParseSignedCompact(t.raw, []jose.SignatureAlgorithm{jose.SignatureAlgorithm(t.algorithm)})
	if e != nil {
		return Identity{}, ErrToken
	}
	body, e := signature.Verify(key.Key)
	if e != nil {
		return Identity{}, ErrToken
	}
	d, e := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxTokenBytes, MaxDepth: 32, MaxNodes: 4096})
	if e != nil {
		return Identity{}, ErrToken
	}
	issuer, _ := stringClaim(d, "/iss")
	subject, ok := stringClaim(d, "/sub")
	if !ok || len(subject) > 1024 || issuer != c.Issuer {
		return Identity{}, ErrToken
	}
	exp, ok := dateClaim(d, "/exp")
	iat, issued := dateClaim(d, "/iat")
	if !ok || !issued || iat > now.Unix() || exp <= now.Unix() || exp <= iat || exp-iat > c.MaxLifetimeSeconds {
		return Identity{}, ErrToken
	}
	if _, exists := d.Lookup("/nbf"); exists {
		nbf, valid := dateClaim(d, "/nbf")
		if !valid || nbf > now.Unix() || nbf >= exp {
			return Identity{}, ErrToken
		}
	}
	aud, exists := d.Lookup("/aud")
	if !exists {
		return Identity{}, ErrToken
	}
	var audiences []string
	if s, ok := aud.Text(); ok {
		audiences = []string{s}
	} else if json.Unmarshal(aud.Bytes(), &audiences) != nil {
		return Identity{}, ErrToken
	}
	if !unique(audiences, 1, 32, 256) {
		return Identity{}, ErrToken
	}
	matched := false
	for _, a := range audiences {
		matched = matched || slices.Contains(c.Audiences, a)
	}
	if !matched {
		return Identity{}, ErrToken
	}
	for _, m := range c.Mappings {
		matches := true
		for p, expected := range m.Match {
			actual, ok := stringClaim(d, p)
			if !ok || actual != expected {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		endUser := ""
		if m.EndUserClaim != nil {
			var ok bool
			endUser, ok = stringClaim(d, *m.EndUserClaim)
			if !ok {
				return Identity{}, ErrToken
			}
		}
		return Identity{Subject: subject, Mapping: m, EndUser: endUser, ExpiresAt: time.Unix(exp, 0).UTC()}, nil
	}
	return Identity{}, ErrToken
}

// ParseKeys accepts public signature keys only, with one unambiguous key per ID.
func ParseKeys(data []byte) (map[string]jose.JSONWebKey, error) {
	if _, e := oif.ParseJSON(data, oif.Limits{MaxBytes: 1 << 20, MaxDepth: 16, MaxNodes: 2048}); e != nil {
		return nil, ErrKeys
	}
	var set jose.JSONWebKeySet
	if json.Unmarshal(data, &set) != nil || len(set.Keys) == 0 || len(set.Keys) > 64 {
		return nil, ErrKeys
	}
	keys := map[string]jose.JSONWebKey{}
	for _, key := range set.Keys {
		if key.KeyID == "" || len(key.KeyID) > 128 || !key.Valid() || !key.IsPublic() || key.Use != "" && key.Use != "sig" {
			return nil, ErrKeys
		}
		if _, ok := keys[key.KeyID]; ok {
			return nil, ErrKeys
		}
		switch k := key.Key.(type) {
		case *rsa.PublicKey:
			if k.N.BitLen() < 2048 || k.N.BitLen() > 8192 {
				return nil, ErrKeys
			}
		case *ecdsa.PublicKey:
			if k.Curve != elliptic.P256() {
				return nil, ErrKeys
			}
		case ed25519.PublicKey:
			if len(k) != ed25519.PublicKeySize {
				return nil, ErrKeys
			}
		default:
			return nil, ErrKeys
		}
		if key.Algorithm != "" && !slices.Contains([]string{"RS256", "ES256", "EdDSA"}, key.Algorithm) {
			return nil, ErrKeys
		}
		keys[key.KeyID] = key
	}
	return keys, nil
}

func (t Token) KeyID() string { return t.kid }

// Allowed may reject unverified input before an outbound key refresh. It never
// grants authority; Verify independently checks signed claims afterward.
func (t Token) Allowed(c Config, now time.Time) bool {
	if t.expires <= now.Unix() || t.issued > now.Unix() || t.expires <= t.issued || t.expires-t.issued > c.MaxLifetimeSeconds || t.notBefore > now.Unix() || t.notBefore >= t.expires {
		return false
	}
	return c.Enabled && t.issuer == c.Issuer && slices.Contains(c.Algorithms, t.algorithm) && !slices.Contains(c.DisabledKeyIDs, t.kid)
}
