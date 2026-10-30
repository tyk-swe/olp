package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestWorkflowRequiresBoundProjectAndRouteRevision(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	key := secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), project)
	store := &WorkflowStore{Pool: &pgxpool.Pool{}, Auth: key}
	for _, test := range []struct {
		name      string
		store     *WorkflowStore
		authority access.Authority
		route     string
		revision  string
	}{
		{name: "missing store", authority: access.Authority{ProjectID: &project}, route: project, revision: project},
		{name: "missing authority", store: store, route: project, revision: project},
		{name: "missing route", store: store, authority: access.Authority{ProjectID: &project}, revision: project},
		{name: "invalid route", store: store, authority: access.Authority{ProjectID: &project}, route: "invalid", revision: project},
		{name: "missing revision", store: store, authority: access.Authority{ProjectID: &project}, route: project},
		{name: "invalid revision", store: store, authority: access.Authority{ProjectID: &project}, route: project, revision: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, err := test.store.Start(context.Background(), test.authority, test.route, test.revision)
			if id != "" || !errors.Is(err, limits.ErrDebitAuthority) {
				t.Fatalf("unbound workflow returned id %q and error %v", id, err)
			}
		})
	}
}

func TestWorkflowApprovalRejectsInvalidRequestsBeforePersistence(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("01", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	store := &WorkflowStore{Pool: &pgxpool.Pool{}, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), project), Keys: ring}
	authority := access.Authority{ProjectID: &project}
	validCall := WorkflowApprovalCall{CallID: "call-1", Arguments: json.RawMessage(`{"term":"example"}`)}
	request := func(calls []WorkflowApprovalCall, transcript, state json.RawMessage, ordinal int, expires time.Time) WorkflowApprovalRequest {
		return WorkflowApprovalRequest{RunID: project, RouteRevisionID: project, StartOrdinal: ordinal, Transcript: transcript, State: state, Calls: calls, ExpiresAt: expires}
	}
	validTranscript := json.RawMessage(`[{"role":"user","content":"example"}]`)
	validState := json.RawMessage(`{"step":1}`)
	tooManyCalls := slices.Collect(func(yield func(WorkflowApprovalCall) bool) {
		for i := range 33 {
			if !yield(WorkflowApprovalCall{CallID: fmt.Sprintf("call-%d", i), Arguments: json.RawMessage(`{}`)}) {
				return
			}
		}
	})
	for _, test := range []struct {
		name    string
		request WorkflowApprovalRequest
	}{
		{name: "negative step", request: request([]WorkflowApprovalCall{validCall}, validTranscript, validState, -1, time.Time{})},
		{name: "no calls", request: request(nil, validTranscript, validState, 0, time.Time{})},
		{name: "too many calls", request: request(tooManyCalls, validTranscript, validState, 0, time.Time{})},
		{name: "invalid transcript", request: request([]WorkflowApprovalCall{validCall}, json.RawMessage(`{`), validState, 0, time.Time{})},
		{name: "invalid state", request: request([]WorkflowApprovalCall{validCall}, validTranscript, json.RawMessage(`{`), 0, time.Time{})},
		{name: "missing call identity", request: request([]WorkflowApprovalCall{{Arguments: json.RawMessage(`{}`)}}, validTranscript, validState, 0, time.Time{})},
		{name: "invalid arguments", request: request([]WorkflowApprovalCall{{CallID: "call-1", Arguments: json.RawMessage(`{`)}}, validTranscript, validState, 0, time.Time{})},
		{name: "duplicate call identity", request: request([]WorkflowApprovalCall{validCall, validCall}, validTranscript, validState, 0, time.Time{})},
		{name: "expired approval", request: request([]WorkflowApprovalCall{validCall}, validTranscript, validState, 0, time.Now().Add(-time.Minute))},
	} {
		t.Run(test.name, func(t *testing.T) {
			token, err := store.Pause(context.Background(), authority, test.request, 4096)
			if token != "" || !errors.Is(err, ErrApproval) {
				t.Fatalf("invalid request returned token %q and error %v", token, err)
			}
		})
	}
}

func TestWorkflowContinuationRejectsUntrustedInput(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	store := &WorkflowStore{Pool: &pgxpool.Pool{}, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), project), Keys: &secrets.KeyRing{}}
	authority := access.Authority{ProjectID: &project}
	continuation, err := store.Continue(context.Background(), authority, "not-a-continuation", json.RawMessage(`[]`), nil, 4096)
	if continuation.RunID != "" || !errors.Is(err, ErrApproval) {
		t.Fatalf("invalid continuation returned run %q and error %v", continuation.RunID, err)
	}
}
