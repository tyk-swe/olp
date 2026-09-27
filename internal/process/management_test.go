package process

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

// Every operation the contract declares must be served by its own handler,
// or its declared requirement would describe a route nobody registered.
func TestEveryContractOperationIsServed(t *testing.T) {
	mux := http.NewServeMux()
	Management{Access: &access.Server{}, Egress: &egress.Policy{}, Log: slog.New(slog.DiscardHandler)}.Register(mux)
	requirements, err := access.ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	for pattern := range requirements {
		method, path, _ := strings.Cut(pattern, " ")
		r := httptest.NewRequest(method, parameter.ReplaceAllString(path, "x"), nil)
		if _, served := mux.Handler(r); served != pattern {
			t.Errorf("%s is served by %q", pattern, served)
		}
	}
}
