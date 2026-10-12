package gateway

import (
	"bytes"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	serving "github.com/tyk-swe/olp/internal/runtime"
)

func TestPreparedAdmissionAndNativeRequestSurviveEviction(t *testing.T) {
	for _, strict := range []bool{false, true} {
		release, parsed, providers := preparationFixture(t, strict, 8, 4096)
		route := release.Snapshot.Routes[routeSlug]
		x := &execution{request: request{release: release}, parsed: parsed, route: &route}
		var bodies [][]byte
		var estimates []admittedEstimate
		for i := range providers {
			prepared, err := x.preparedProvider(&providers[i], "model")
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, prepared.invocation.Prepared.Document().Bytes())
			estimates = append(estimates, prepared.admitted)
			x.attempts = append(x.attempts, serving.Attempt{ProviderID: providers[i].ID, ProviderRevisionID: providers[i].RevisionID, UpstreamModel: "model"})
		}
		x.finishPreparationPlanning()
		// Include targets beyond the retained first attempts, and revisit a
		// previous target after another attempt took ownership of dispatch.
		for _, i := range []int{7, 0, 5, 0} {
			x.activatePreparation(&providers[i], "model")
			if got, err := x.attemptEstimate(&providers[i], "model"); err != nil || got != estimates[i] {
				t.Fatalf("admission changed after eviction: %+v %v", got, err)
			}
			prepared, err := x.preparedProvider(&providers[i], "model")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(prepared.invocation.Prepared.Document().Bytes(), bodies[i]) {
				t.Fatal("rebuilt native request changed")
			}
		}
	}
}

func TestEffectiveSourceChangeReplacesAllAdmissionState(t *testing.T) {
	release, parsed, providers := preparationFixture(t, false, 1, 128)
	route := release.Snapshot.Routes[routeSlug]
	x := &execution{request: request{release: release}, parsed: parsed, route: &route}
	first, err := x.attemptEstimate(&providers[0], "model")
	if err != nil {
		t.Fatal(err)
	}
	x.parsed, err = openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"changed"}],"stream":true,"max_tokens":2048}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := x.attemptEstimate(&providers[0], "model")
	if err != nil {
		t.Fatal(err)
	}
	if next.reply == first.reply || next.reply != 2048 {
		t.Fatalf("old source admission survived rewrite: before=%+v after=%+v", first, next)
	}
	prepared, err := x.preparedProvider(&providers[0], "model")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prepared.invocation.Prepared.Document().Bytes(), []byte(`"max_tokens":2048`)) {
		t.Fatal("dispatch rebuilt an earlier source")
	}
}
