package main

import (
	"github.com/tyk-swe/olp/internal/grokauth"
	"github.com/tyk-swe/olp/plugin"
)

func init() { plugin.Register(grokauth.Adapter{Fetch: plugin.Fetch}) }
func main() { plugin.Serve() }
