// Command fixture is a provider plugin that misbehaves on purpose. The linker
// sets how it reports its manifest, so one package builds every fixture the
// tests need:
//
//	go build -buildmode=c-shared -ldflags=-X=main.behaviour=loop ...
//
// Its profile signs requests, and the credential of a request sets how, so
// one build serves every signing test; likewise a grant's refresh token sets
// how it refreshes. It builds natively too, as an unconfined plugin.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Secret is the value the log and panic behaviours leak.
const secret = "sk-fixture-secret"

var behaviour string
var upstream = "https://api.example.com/v1"

type fixture struct{}

func (fixture) Manifest() plugin.Manifest {
	m := plugin.Manifest{
		Name:    "fixture",
		Version: "1.0.0",
		Origins: []string{strings.TrimSuffix(upstream, "/v1")},
		Profiles: []plugin.Profile{{ID: "fixture-chat", Label: "Fixture Chat", Dialect: "openai-chat", Hosting: plugin.Hosting{
			Address: upstream, Headers: map[string]string{"Authorization": "Bearer {credential}"},
		}, Signing: true}},
	}
	switch behaviour {
	case "unknown-dialect":
		m.Profiles[0].Dialect = "fixture-chat"
	case "invalid-manifest":
		m.Origins = []string{"https://api.example.com/v1"}
	case "loop":
		for {
		}
	case "allocate":
		var held [][]byte
		for {
			held = append(held, make([]byte, 1<<20))
		}
	case "log":
		plugin.Log.Info("using "+secret, "credential", secret)
		fmt.Fprintln(os.Stderr, "stderr "+secret)
	case "panic":
		panic("fixture panicked holding " + secret)
	}
	return m
}

// calls counts the requests this instance of the module, or this process,
// has signed.
var calls atomic.Int64

// Sign signs as the credential's prefix up to a colon says: "loop" never
// returns, "allocate" exhausts memory, "exit" stops the plugin, "fail" reports
// a failure holding the credential, "large-failure:N" logs N records and
// reports oversized failure diagnostics, "log" logs the credential, "stderr" writes
// the credential to standard error just after it answers, "wait" returns once
// its call is cancelled, "header:Name" returns that header, "credential-header"
// returns a reserved header containing the credential in its name,
// and "option:name" returns X-Fixture-Option, the provider's profile and value
// of that option. Anything else returns X-Fixture-Signature and
// X-Fixture-Calls, this instance's count of signed requests.
func (fixture) Sign(ctx context.Context, r plugin.SignRequest) (plugin.SignResult, error) {
	called := calls.Add(1)
	behaviour, rest, _ := strings.Cut(r.Credential, ":")
	switch behaviour {
	case "loop":
		for {
		}
	case "allocate":
		var held [][]byte
		for {
			held = append(held, make([]byte, 1<<20))
		}
	case "exit":
		os.Exit(3)
	case "fail":
		return plugin.SignResult{}, &plugin.Error{Code: "fixture_failed", Message: "signing with " + r.Credential + " failed"}
	case "large-failure":
		records, _ := strconv.Atoi(rest)
		for range records {
			plugin.Log.InfoContext(ctx, strings.Repeat("x", 2<<10))
		}
		return plugin.SignResult{}, &plugin.Error{Code: secret + strings.Repeat("failure", 4<<10), Message: secret + strings.Repeat("é", 32<<10)}
	case "log":
		plugin.Log.InfoContext(ctx, "signing with "+r.Credential, "url", r.URL)
	case "stderr":
		go func() {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprintln(os.Stderr, "signed with "+r.Credential)
		}()
	case "wait":
		<-ctx.Done()
		return plugin.SignResult{}, &plugin.Error{Code: "fixture_cancelled", Message: "the call was cancelled"}
	case "header":
		return plugin.SignResult{Headers: map[string]string{rest: "fixture"}}, nil
	case "credential-header":
		return plugin.SignResult{Headers: map[string]string{"X-OLP-" + r.Credential: "fixture"}}, nil
	case "option":
		provider, ok := plugin.ProviderOf(ctx)
		if !ok {
			return plugin.SignResult{}, &plugin.Error{Code: "fixture_failed", Message: "the call serves no provider"}
		}
		return plugin.SignResult{Headers: map[string]string{"X-Fixture-Option": provider.Profile + " " + provider.Options[rest]}}, nil
	}
	return plugin.SignResult{Headers: map[string]string{
		"X-Fixture-Signature": r.Method + " " + r.URL + " " + strconv.Itoa(len(r.Body)),
		"X-Fixture-Calls":     strconv.FormatInt(called, 10),
	}}, nil
}

// RefreshGrant refreshes as the refresh token's prefix up to a colon says:
// "log" logs the refresh token, and "fetch:URL" GETs the URL and returns its
// body as the access token. Anything else reports invalid_grant.
func (fixture) RefreshGrant(ctx context.Context, r plugin.GrantRefresh) (plugin.Grant, error) {
	behaviour, rest, _ := strings.Cut(r.RefreshToken, ":")
	switch behaviour {
	case "log":
		plugin.Log.InfoContext(ctx, "refreshing with "+r.RefreshToken, "profile", r.Profile)
		return plugin.Grant{AccessToken: "refreshed"}, nil
	case "fetch":
		response, err := plugin.Fetch(ctx, plugin.HTTPRequest{Method: "GET", URL: rest})
		if err != nil {
			return plugin.Grant{}, err
		}
		return plugin.Grant{AccessToken: string(response.Body)}, nil
	}
	return plugin.Grant{}, &plugin.Error{Code: abi.CodeInvalidGrant, Message: "the refresh token is spent"}
}

func init() { plugin.Register(fixture{}) }

func main() { plugin.Serve() }
