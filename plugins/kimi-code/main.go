package main

import (
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/sdk/plugin"
)

func init() { plugin.Register(codeplans.Kimi()) }

func main() { plugin.Serve() }
