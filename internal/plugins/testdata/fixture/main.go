// Command fixture is a provider plugin that misbehaves on purpose. The linker
// sets its behaviour, so one package builds every fixture the tests need:
//
//	go build -buildmode=c-shared -ldflags=-X=main.behaviour=loop ...
package main

import (
	"fmt"
	"os"

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
		}}},
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

func init() { plugin.Register(fixture{}) }

func main() {}
