// Package fixtures preserves the language-neutral reference corpus used by
// the routing, stream, and token tests.
package fixtures

import "embed"

//go:embed routing/attempt-order.json streams/*.sse tokens/*.json vendors
var Files embed.FS
