package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codemode/codeadmin"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/codesessions/codeaccounts"
)

func (m Management) registerCode(mux *http.ServeMux) {
	codeaccounts.NewAPI(m.Access).Register(mux)
	codeadmin.NewAPI(m.Access).Register(mux)
	codesessions.NewManagement(m.Access).Register(mux)
}
