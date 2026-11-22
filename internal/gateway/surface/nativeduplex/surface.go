package nativeduplex

import (
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

var (
	errDuplexProviderError  = errors.New("native duplex provider reported an error")
	errDuplexProtocol       = errors.New("native duplex provider protocol violation")
	errDuplexClientProtocol = errors.New("native duplex client protocol violation")
	errDuplexAuthority      = errors.New("native duplex authority revoked")
	errDuplexClientClosed   = errors.New("native duplex client closed with work in flight")
	errDuplexIncomplete     = errors.New("native duplex provider closed with work in flight")
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	s := surface{kernel: k}
	mux.HandleFunc("GET /native/{dialect}/sessions/{route}", s.duplex)
}
