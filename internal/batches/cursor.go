package batches

import "errors"

const (
	MaxLineBytes  = 1 << 20
	MaxBatchBytes = 512 << 20
	PageSize      = 100
	MaxPageBytes  = 1 << 20
)

var (
	ErrCursorConflict = errors.New("batch result prefix changed")
	ErrLineTooLarge   = errors.New("batch result line exceeds the limit")
	ErrBatchTooLarge  = errors.New("batch result files exceed the limit")
)

type Phase string

const (
	PhaseStart      Phase = ""
	PhaseOutput     Phase = "output"
	PhaseError      Phase = "error"
	PhaseVertexDone Phase = "vertex_done"
	PhaseDone       Phase = "done"
)

type Cursor struct {
	Phase              Phase  `json:"phase,omitempty"`
	File               string `json:"file,omitempty"`
	Line               int64  `json:"line,omitempty"`
	PrefixSHA256       string `json:"prefix_sha256,omitempty"`
	OutputPrefixSHA256 string `json:"output_prefix_sha256,omitempty"`
	VertexBytes        *int64 `json:"vertex_bytes,omitempty"`
}
