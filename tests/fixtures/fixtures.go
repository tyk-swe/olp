// Package fixtures preserves the language-neutral reference corpus. Provider
// runners are attached as their owning milestones land.
package fixtures

import "embed"

//go:embed protocols/*.json routing/*.json security/*.json streams/*.sse streams/*.json
var Files embed.FS
