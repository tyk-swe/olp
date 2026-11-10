package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/tests/harness/vendor"
)

type fixture struct {
	Secret       string `json:"secret"`
	Model        string `json:"model"`
	Answer       string `json:"answer"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
}

func main() {
	address := flag.String("listen", "127.0.0.1:0", "address the scripted vendor listens on")
	flag.Parse()
	mux := http.NewServeMux()
	mux.Handle("/", vendor.Handler())
	mux.HandleFunc("GET /fixture", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(fixture{
			Secret:       vendor.Secret,
			Model:        vendor.Model,
			Answer:       vendor.Answer,
			InputTokens:  vendor.InputTokens,
			OutputTokens: vendor.OutputTokens,
		}); err != nil {
			log.Print(err)
		}
	})
	server := &http.Server{Addr: *address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
