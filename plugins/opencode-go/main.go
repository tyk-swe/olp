package main

import (
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/sdk/plugin"
)

func init() { plugin.Register(codeplans.OpenCodeGo()) }

func main() { plugin.Serve() }
