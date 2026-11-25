package main

import (
	"github.com/tyk-swe/olp/internal/codemode/codeplans"
	"github.com/tyk-swe/olp/plugin"
)

func init() { plugin.Register(codeplans.ZAI()) }

func main() { plugin.Serve() }
