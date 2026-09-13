// Package openapi embeds the checked-in management contract, independently of
// application composition and generated Go transport types.
package openapi

import _ "embed"

//go:embed management.json
var Document []byte
