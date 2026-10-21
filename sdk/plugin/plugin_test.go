package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type manifestOnly Manifest

func (m manifestOnly) Manifest() Manifest { return Manifest(m) }

type signing struct {
	manifestOnly
	sign func(context.Context, SignRequest) (SignResult, error)
}

func (s signing) Sign(ctx context.Context, r SignRequest) (SignResult, error) { return s.sign(ctx, r) }

type routing struct {
	manifestOnly
	match func(RouteFeatures) bool
}

func (r routing) MatchRoute(_ context.Context, features RouteFeatures) (RouteVerdict, error) {
	return RouteVerdict{Match: r.match(features)}, nil
}

type panicking struct{}

func (panicking) Manifest() Manifest { panic("declared nothing") }

func serveWith(t *testing.T, p Plugin, request string) abi.Response {
	t.Helper()
	registered = p
	t.Cleanup(func() { registered = nil })
	var response abi.Response
	if err := json.Unmarshal(serve([]byte(request)), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// A plugin whose profiles authenticate with static credentials enrolls
// no grants.

// A plugin whose grants carry no refresh token refreshes none.

func TestServeReportsFailuresAsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		plugin  Plugin
		request string
		code    string
	}{
		"unknown method":     {manifestOnly{}, `{"method":"sign"}`, abi.CodeUnknownMethod},
		"malformed":          {manifestOnly{}, `{"method":`, abi.CodeInvalidRequest},
		"nothing registered": {nil, `{"method":"manifest"}`, abi.CodeInternal},
		"panic":              {panicking{}, `{"method":"manifest"}`, abi.CodeInternal},
		"malformed sign":     {signing{}, `{"method":"sign","params":[]}`, abi.CodeInvalidRequest},
		"reported": {signing{sign: func(context.Context, SignRequest) (SignResult, error) {
			return SignResult{}, &Error{Code: "expired", Message: "The key expired."}
		}}, `{"method":"sign","params":{}}`, "expired"},
		"failed": {signing{sign: func(context.Context, SignRequest) (SignResult, error) { return SignResult{}, errors.New("no clock") }}, `{"method":"sign","params":{}}`, abi.CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			if response := serveWith(t, tc.plugin, tc.request); response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("want %s, got %+v", tc.code, response)
			}
		})
	}
}
