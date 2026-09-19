// Package fixtures preserves the language-neutral reference corpus used by
// protocol, routing, and egress conformance tests.
package fixtures

import "embed"

//go:embed protocols/*.json routing/*.json security/*.json streams/*.sse streams/*.json
var Files embed.FS
