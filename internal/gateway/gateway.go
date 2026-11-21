// Package gateway assembles the execution kernel and the surfaces built on it.
package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/anthropicstate"
	"github.com/tyk-swe/olp/internal/gateway/surface/bedrock"
	"github.com/tyk-swe/olp/internal/gateway/surface/cloudbatches"
	"github.com/tyk-swe/olp/internal/gateway/surface/geminiinteractions"
	"github.com/tyk-swe/olp/internal/gateway/surface/geminilive"
	"github.com/tyk-swe/olp/internal/gateway/surface/geministate"
	"github.com/tyk-swe/olp/internal/gateway/surface/guardrails"
	"github.com/tyk-swe/olp/internal/gateway/surface/media"
	"github.com/tyk-swe/olp/internal/gateway/surface/nativeduplex"
	"github.com/tyk-swe/olp/internal/gateway/surface/openaistate"
	"github.com/tyk-swe/olp/internal/gateway/surface/passthrough"
	"github.com/tyk-swe/olp/internal/gateway/surface/realtime"
	"github.com/tyk-swe/olp/internal/gateway/surface/retrieval"
)

func Register(k *kernel.Server, mux *http.ServeMux) {
	k.RegisterKernel(mux, geministate.Actions(k), retrieval.Handler(k))
	anthropicstate.Register(k, mux)
	cloudbatches.Register(k, mux)
	geminiinteractions.Register(k, mux)
	geministate.Register(k, mux)
	guardrails.Register(k, mux)
	geminilive.Register(k, mux)
	retrieval.Register(k, mux)
	media.Register(k, mux)
	nativeduplex.Register(k, mux)
	openaistate.Register(k, mux)
	passthrough.Register(k, mux)
	bedrock.Register(k, mux)
	realtime.Register(k, mux)
}
