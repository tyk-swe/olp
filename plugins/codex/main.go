package main

import (
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/sdk/plugin"
)

func init() { plugin.Register(codexauth.Adapter{Fetch: plugin.Fetch}) }

func main() { plugin.Serve() }
