package main

import (
	"github.com/tyk-swe/olp/internal/codemode/codexauth"
	"github.com/tyk-swe/olp/plugin"
)

func init() { plugin.Register(codexauth.Adapter{Fetch: plugin.Fetch}) }

func main() { plugin.Serve() }
