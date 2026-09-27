// Command fixture is a provider plugin that misbehaves on purpose. The linker
// sets how it reports its manifest, so one package builds every fixture the
// tests need:
//
//	go build -buildmode=c-shared -ldflags=-X=main.behaviour=loop ...
//
// Its profile signs requests, and the credential of a request sets how, so
// one build serves every signing test.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/sdk/plugin"
)

// Secret is the value the log and panic behaviours leak.
const secret = "sk-fixture-secret"

var behaviour string

type fixture struct{}

func (fixture) Manifest() plugin.Manifest {
	m := plugin.Manifest{
		Name:    "fixture",
		Version: "1.0.0",
		Origins: []string{"https://api.example.com"},
		Profiles: []plugin.Profile{{ID: "fixture-chat", Label: "Fixture Chat", Dialect: "openai-chat", Hosting: plugin.Hosting{
			Address: "https://api.example.com/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"},
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

// calls counts the requests this instance of the module has signed.
var calls int

// Sign signs as the credential's prefix up to a colon says: "loop" never
// returns, "allocate" exhausts memory, "fail" reports a failure holding the
// credential, "log" logs the credential, "header:Name" returns that header,
// and "option:name" returns X-Fixture-Option, the provider's profile and value
// of that option. Anything else returns X-Fixture-Signature and
// X-Fixture-Calls, this instance's count of signed requests.
func (fixture) Sign(ctx context.Context, r plugin.SignRequest) (plugin.SignResult, error) {
	calls++
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
	case "fail":
		return plugin.SignResult{}, &plugin.Error{Code: "fixture_failed", Message: "signing with " + r.Credential + " failed"}
	case "log":
		plugin.Log.Info("signing with "+r.Credential, "url", r.URL)
	case "header":
		return plugin.SignResult{Headers: map[string]string{rest: "fixture"}}, nil
	case "option":
		provider, ok := plugin.ProviderOf(ctx)
		if !ok {
			return plugin.SignResult{}, &plugin.Error{Code: "fixture_failed", Message: "the call serves no provider"}
		}
		return plugin.SignResult{Headers: map[string]string{"X-Fixture-Option": provider.Profile + " " + provider.Options[rest]}}, nil
	}
	return plugin.SignResult{Headers: map[string]string{
		"X-Fixture-Signature": r.Method + " " + r.URL + " " + strconv.Itoa(len(r.Body)),
		"X-Fixture-Calls":     strconv.Itoa(calls),
	}}, nil
}

func init() { plugin.Register(fixture{}) }

func main() {}
