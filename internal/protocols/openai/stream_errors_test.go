package openai

import ()

type failedStreamReader struct{ err error }

func (r failedStreamReader) Read([]byte) (int, error) { return 0, r.err }

// Matching text in an I/O error must not turn it into a framing error.
