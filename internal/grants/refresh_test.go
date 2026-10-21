package grants

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A refresh renews the grant's own account: a refresh that observes another
// principal or reports other grant facts, like one the upstream refuses with
// invalid_grant, a plugin that refreshes no grants or one no longer installed
// or approved, ends the grant's refresh. Other failures are retried only
// when OLP knows the token was not spent.
func TestRefreshFailuresArePermanentOnlyWhenTheGrantCanNoLongerRefresh(t *testing.T) {
	g := &dueGrant{principal: "user@acme.example", facts: map[string]string{"account": "7"}}
	for name, grant := range map[string]abi.Grant{
		"tokens only":     {AccessToken: "at-2"},
		"same account":    {AccessToken: "at-2", RefreshToken: "rt-2", ExpiresIn: 3600, Principal: "user@acme.example", Facts: map[string]string{"account": "7"}},
		"principal alone": {AccessToken: "at-2", Principal: "user@acme.example"},
	} {
		if err := checkRefreshed(g, grant); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		grant     abi.Grant
		permanent bool
	}{
		"another principal":  {abi.Grant{AccessToken: "at-2", Principal: "other@acme.example"}, true},
		"other facts":        {abi.Grant{AccessToken: "at-2", Facts: map[string]string{"account": "8"}}, true},
		"no access token":    {abi.Grant{RefreshToken: "rt-2"}, false},
		"access token line":  {abi.Grant{AccessToken: "at\r\nX-Injected: 1"}, false},
		"long refresh token": {abi.Grant{AccessToken: "at-2", RefreshToken: strings.Repeat("r", maxToken+1)}, false},
	} {
		err := checkRefreshed(g, tc.grant)
		if err == nil || permanent(err) != tc.permanent {
			t.Errorf("%s: %v (permanent %v)", name, err, permanent(err))
		}
	}
	for err, want := range map[error]bool{
		&abi.Error{Code: abi.CodeInvalidGrant, Message: "revoked"}:        true,
		&abi.Error{Code: abi.CodeUnknownMethod, Message: "no refresh"}:    true,
		&abi.Error{Code: abi.CodeHTTPFailed, Message: "unreachable"}:      false,
		&abi.Error{Code: "temporarily_unavailable", Message: "try later"}: false,
		&plugins.Error{Code: plugins.CodeTimedOut, Message: "time limit"}: false,
		fmt.Errorf("wrapped: %w", &abi.Error{Code: abi.CodeInvalidGrant}): true,
		// The grant's plugin is gone from this installation; a deployment's
		// configuration or image may bring an unconfined one back.
		&plugins.Error{Code: plugins.CodeNotInstalled}:                           true,
		fmt.Errorf("wrapped: %w", &plugins.Error{Code: plugins.CodeNotApproved}): true,
		&plugins.Error{Code: plugins.CodeUnconfinedDisabled}:                     false,
		&plugins.Error{Code: plugins.CodeExecutableChanged}:                      false,
		&plugins.Error{Code: plugins.CodeExecutableInvalid}:                      false,
	} {
		if permanent(err) != want {
			t.Errorf("%v: permanent %v", err, !want)
		}
	}
}
