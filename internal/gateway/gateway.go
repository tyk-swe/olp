// Package gateway assembles the execution kernel and the surfaces built on it.
package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/anthropicstate"
	"github.com/tyk-swe/olp/internal/gateway/surface/cloudbatches"
	"github.com/tyk-swe/olp/internal/gateway/surface/geminiinteractions"
	"github.com/tyk-swe/olp/internal/gateway/surface/geministate"
)

func Register(k *kernel.Server, mux *http.ServeMux) {
	k.RegisterKernel(mux, geministate.Actions(k))
	anthropicstate.Register(k, mux)
	cloudbatches.Register(k, mux)
	geminiinteractions.Register(k, mux)
	geministate.Register(k, mux)
}
