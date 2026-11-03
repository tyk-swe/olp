// Package fixtures preserves the language-neutral reference corpus used by
// the protocol, routing, and token tests in the fast safety net.
package fixtures

import "embed"

//go:embed protocols/*.json routing/*.json security/*.json streams/*.sse streams/*.json tokens/*.json vendors/*/*.json vendors/*/*.jsonl vendors/*/*.sse vendors/*/*.out
var Files embed.FS
