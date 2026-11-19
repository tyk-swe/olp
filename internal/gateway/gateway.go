// Package gateway assembles the execution kernel and the surfaces built on it.
package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/anthropicstate"
)

func Register(k *kernel.Server, mux *http.ServeMux) {
	k.RegisterKernel(mux)
	anthropicstate.Register(k, mux)
}
