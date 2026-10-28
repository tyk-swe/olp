package workload

import (
	"context"
	"strings"
	"time"
)

type assertionKey struct{}

type UserAssertion struct {
	raw     string
	expires time.Time
}

func WithAssertion(ctx context.Context, raw string) context.Context {
	return context.WithValue(ctx, assertionKey{}, raw)
}

func RequestAssertion(ctx context.Context) string {
	value, _ := ctx.Value(assertionKey{}).(string)
	return value
}

func (a UserAssertion) Token() string        { return a.raw }
func (a UserAssertion) ExpiresAt() time.Time { return a.expires }
func (a UserAssertion) String() string       { return "[redacted user assertion]" }

func (t Token) EntraUser(issuer, audience string) (UserAssertion, error) {
	if t.issuer != issuer || !time.Unix(t.expires, 0).After(time.Now()) {
		return UserAssertion{}, ErrToken
	}
	document, err := claims(strings.Split(t.raw, ".")[1])
	if err != nil {
		return UserAssertion{}, ErrToken
	}
	actualAudience, audienceOK := stringClaim(document, "/aud")
	scopes, scopesOK := stringClaim(document, "/scp")
	if !audienceOK || actualAudience != audience || !scopesOK || len(scopes) > 4096 || len(strings.Fields(scopes)) == 0 {
		return UserAssertion{}, ErrToken
	}
	if _, present := document.Lookup("/idtyp"); present {
		kind, ok := stringClaim(document, "/idtyp")
		if !ok || kind != "user" {
			return UserAssertion{}, ErrToken
		}
	}
	return UserAssertion{raw: t.raw, expires: time.Unix(t.expires, 0)}, nil
}
