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
	"github.com/tyk-swe/olp/internal/gateway/surface/retained"
	"github.com/tyk-swe/olp/internal/gateway/surface/retrieval"
)

type Option func(*options)

type options struct {
	media *media.Deps
}

func WithMedia(deps media.Deps) Option {
	return func(o *options) { o.media = &deps }
}

func Register(k *kernel.Server, mux *http.ServeMux, deps kernel.Dependencies, opts ...Option) {
	var cfg options
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	k.RegisterKernel(mux, geministate.Actions(k, deps.Resources, deps.Staging))
	anthropicstate.Register(k, mux, deps.Resources, deps.Staging)
	cloudbatches.Register(k, mux, deps.Resources)
	geminiinteractions.Register(k, mux, deps.Resources, deps.Resolver)
	geministate.Register(k, mux, deps.Resources, deps.Staging)
	guardrails.Register(k, mux)
	geminilive.Register(k, mux)
	retrieval.Register(k, mux, deps.Resources)
	retained.Register(k, mux, deps.Resources, deps.Admission, retrieval.Handler(k, deps.Resources))
	media.Register(k, mux, cfg.media, deps.Staging, deps.Admission)
	nativeduplex.Register(k, mux)
	openaistate.Register(k, mux, deps.Resources, deps.Staging, deps.Batches)
	passthrough.Register(k, mux)
	bedrock.Register(k, mux, deps.Runtime, deps.Resources)
	realtime.Register(k, mux, deps.Resources)
}
