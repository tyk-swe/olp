package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestJSONLogPreservesEveryAttempt(t *testing.T) {
	for _, facts := range [][]AttemptFact{
		nil,
		{{Ordinal: 1, ProviderID: "first", TargetID: "target-a", SlotID: "slot-a", CredentialVersion: 1, Status: 503, Class: classUpstreamServer, Duration: time.Millisecond},
			{Ordinal: 2, ProviderID: "second", TargetID: "target-b", SlotID: "slot-b", CredentialVersion: 2, Status: 200, Class: classSuccess, Committed: true, Duration: 2 * time.Millisecond}},
	} {
		var buffer bytes.Buffer
		LogSink{Log: slog.New(slog.NewJSONHandler(&buffer, nil))}.Terminal(Envelope{RequestID: "request", Attempts: facts})
		var record struct {
			Count    int `json:"attempt_count"`
			Attempts []struct {
				Ordinal           int           `json:"ordinal"`
				ProviderID        string        `json:"provider_id"`
				TargetID          string        `json:"target_id"`
				SlotID            string        `json:"slot_id"`
				CredentialVersion int           `json:"credential_version"`
				Status            int           `json:"status"`
				Class             string        `json:"class"`
				Committed         bool          `json:"committed"`
				Duration          time.Duration `json:"duration"`
			} `json:"attempts"`
		}
		if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record.Count != len(facts) || len(record.Attempts) != len(facts) || record.Attempts == nil {
			t.Fatalf("attempts lost during JSON decoding: %s", buffer.Bytes())
		}
		for i, want := range facts {
			got := record.Attempts[i]
			if got.Ordinal != want.Ordinal || got.ProviderID != want.ProviderID || got.TargetID != want.TargetID || got.SlotID != want.SlotID || got.CredentialVersion != want.CredentialVersion || got.Status != want.Status || got.Class != want.Class || got.Committed != want.Committed || got.Duration != want.Duration {
				t.Fatalf("attempt %d changed: %+v, want %+v", i, got, want)
			}
		}
	}
}

func TestJSONLogPreservesActorIdentity(t *testing.T) {
	for _, env := range []Envelope{
		{Actor: "playground", UserID: "user-one"},
		{Actor: "playground", UserID: "user-two"},
		{Actor: "api_key", KeyID: "key-one"},
	} {
		t.Run(env.Actor+"/"+env.UserID+env.KeyID, func(t *testing.T) {
			var buffer bytes.Buffer
			LogSink{Log: slog.New(slog.NewJSONHandler(&buffer, nil))}.Terminal(env)
			var record struct {
				Actor  string `json:"actor"`
				KeyID  string `json:"key_id"`
				UserID string `json:"user_id"`
			}
			if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record.Actor != env.Actor || record.KeyID != env.KeyID || record.UserID != env.UserID {
				t.Fatalf("actor identity lost: %s", buffer.Bytes())
			}
		})
	}
}

func TestJSONLogPreservesRevisionAndUsageMetadata(t *testing.T) {
	var buffer bytes.Buffer
	usage := &openai.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := Envelope{RequestID: "request", ClientIP: "203.0.113.1", RouteRevisionID: "route-revision", StartedAt: at, Usage: usage,
		Attempts: []AttemptFact{{ProviderRevisionID: "provider-revision", CredentialID: "credential-version-id", UpstreamModel: "upstream-model", StartedAt: at, Usage: usage}}}
	LogSink{Log: slog.New(slog.NewJSONHandler(&buffer, nil))}.Terminal(env)
	var got struct {
		ClientIP        string        `json:"client_ip"`
		RouteRevisionID string        `json:"route_revision_id"`
		StartedAt       time.Time     `json:"started_at"`
		Usage           *openai.Usage `json:"usage"`
		Attempts        []struct {
			ProviderRevisionID string        `json:"provider_revision_id"`
			CredentialID       string        `json:"credential_id"`
			UpstreamModel      string        `json:"upstream_model"`
			StartedAt          time.Time     `json:"started_at"`
			Usage              *openai.Usage `json:"usage"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(buffer.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ClientIP != env.ClientIP || got.RouteRevisionID != env.RouteRevisionID || !got.StartedAt.Equal(at) || got.Usage == nil || *got.Usage != *usage || len(got.Attempts) != 1 {
		t.Fatalf("terminal metadata lost: %s", buffer.Bytes())
	}
	a := got.Attempts[0]
	if a.ProviderRevisionID != env.Attempts[0].ProviderRevisionID || a.CredentialID != env.Attempts[0].CredentialID || a.UpstreamModel != env.Attempts[0].UpstreamModel || !a.StartedAt.Equal(at) || a.Usage == nil || *a.Usage != *usage {
		t.Fatalf("attempt metadata lost: %s", buffer.Bytes())
	}
}
