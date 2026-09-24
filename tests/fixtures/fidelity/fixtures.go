// Package fidelity contains independently authored, versioned reference inputs.
// Add a new version when a contract changes; never rewrite a frozen reference
// to match the mapper being evaluated.
package fidelity

import "embed"

//go:embed v1/*.json v1/*.sse
var Files embed.FS
