//go:build integration

package gateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/runtime"
)

func TestCapacitySharesHoldConnectionHeadroomForPriorityWorkAcrossGateways(t *testing.T) {
	h := newHarness(t, Config{})
	limiter := mediaLimiter(t)
	log := slog.New(slog.DiscardHandler)
	other, otherSink, otherServer := h.replica()
	h.gateway.Admission = NewAdmission(limiter, nil, log)
	other.Admission = NewAdmission(limiter, nil, log)
	authority := h.rt.keys[fullKey]
	authority.Policy.MaxPriority = new("critical")
	h.rt.keys[fullKey] = authority

	// Connection a serves two requests at once. Either class may take the
	// idle first half; beyond it low work has no share and critical work has
	// half of the connection.
	h.republish(func(s *runtime.Snapshot, a, _ runtime.Provider) {
		a.Limits = &runtime.Limits{MaxConcurrency: new(int64(2)), Supply: runtime.Supply{
			PriorityShares:    &runtime.Shares{Critical: 50, High: 25, Normal: 25},
			SaturationPercent: new(int64(50)),
		}}
		s.Providers[a.ID] = a
	})
	hold := make(chan struct{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct{ User string }
		json.NewDecoder(r.Body).Decode(&request)
		if request.User == "held" {
			<-hold
		}
		completion(modelA, answerText)(w, r)
	})
	send := func(url, user, class string) int {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"user":"`+user+`"}`))
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(routingHeader, `{"priority":"`+class+`"}`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Low work on the first gateway takes the idle half of a and keeps it.
	held := make(chan int, 1)
	go func() { held <- send(h.server.URL, "held", "low") }()
	for deadline := time.Now().Add(5 * time.Second); h.mock.count("a") == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the held request never reached a")
		}
	}

	// On the other gateway, more low work finds a saturated and is served by
	// b, while critical work is still admitted to its share of a.
	if status := send(otherServer.URL, "low", "low"); status != http.StatusOK {
		t.Fatalf("low status = %d, want failover to b", status)
	}
	attempts := otherSink.last(t).Attempts
	if len(attempts) != 2 || attempts[0].Class != classRateLimit || attempts[1].ProviderID == attempts[0].ProviderID {
		t.Fatalf("low attempts = %+v, want a refused by its share and b serving", attempts)
	}
	if status := send(otherServer.URL, "critical", "critical"); status != http.StatusOK {
		t.Fatalf("critical status = %d", status)
	}
	if attempts := otherSink.last(t).Attempts; len(attempts) != 1 || attempts[0].ProviderID != h.rt.release.Snapshot.Routes[routeSlug].Targets[0].ProviderID {
		t.Fatalf("critical attempts = %+v, want a serving within its share", attempts)
	}
	if h.mock.count("a") != 2 {
		t.Fatalf("a served %d requests, want the held one and the critical one", h.mock.count("a"))
	}
	close(hold)
	if status := <-held; status != http.StatusOK {
		t.Fatalf("held status = %d", status)
	}
}
