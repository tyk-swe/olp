package main

import (
	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/sdk/plugin"
)

func init() { plugin.Register(claudeauth.Adapter{Fetch: plugin.Fetch}) }

func main() { plugin.Serve() }
