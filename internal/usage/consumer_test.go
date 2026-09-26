package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

func TestValidateConsumerConfigRejectsUnrunnableSettings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		stream   string
		consumer string
		mutate   func(*consumerPolicy)
		wantErr  bool
	}{
		{name: "the production policy runs", stream: "olp:x:request-metadata",
			consumer: "host-1-abc"},
		{name: "a stream must be named", stream: "  ", consumer: "host-1-abc", wantErr: true},
		{name: "a consumer must be named", stream: "stream", consumer: " ", wantErr: true},
		{name: "a batch must fit one read", stream: "stream", consumer: "host-1-abc",
			mutate: func(p *consumerPolicy) { p.batchSize = 0 }, wantErr: true},
		{name: "a batch may not exceed the protocol bound", stream: "stream", consumer: "host-1-abc",
			mutate: func(p *consumerPolicy) { p.batchSize = 1001 }, wantErr: true},
		{name: "a block interval must advance the loop", stream: "stream", consumer: "host-1-abc",
			mutate: func(p *consumerPolicy) { p.blockInterval = 0 }, wantErr: true},
		{name: "a health interval must advance the loop", stream: "stream", consumer: "host-1-abc",
			mutate: func(p *consumerPolicy) { p.healthInterval = 0 }, wantErr: true},
		{name: "an immediate reclaim is allowed for tests", stream: "stream",
			consumer: "host-1-abc", mutate: func(p *consumerPolicy) { p.reclaimIdle = 0 }},
		{name: "a negative reclaim idle is not a duration", stream: "stream",
			consumer: "host-1-abc", mutate: func(p *consumerPolicy) { p.reclaimIdle = -time.Second },
			wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policy := defaultConsumerPolicy()
			if test.mutate != nil {
				test.mutate(&policy)
			}
			err := validateConsumerConfig(test.stream, test.consumer, policy)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateConsumerConfig = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestReplyFieldsAcceptsBothProtocolShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		reply   any
		want    map[string]any
		wantErr bool
	}{
		{name: "a resp3 map is already keyed",
			reply: map[string]any{"lag": int64(4)}, want: map[string]any{"lag": int64(4)}},
		{name: "a resp2 flat list is paired",
			reply: []any{"name", "olp:persistence", "lag", int64(4)},
			want:  map[string]any{"name": "olp:persistence", "lag": int64(4)}},
		{name: "an odd list is not a field list",
			reply: []any{"name"}, wantErr: true},
		{name: "a non-string field name is not a field list",
			reply: []any{int64(1), "value"}, wantErr: true},
		{name: "a scalar is not a field container", reply: "lag", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fields, err := replyFields(test.reply)
			if test.wantErr {
				if !errors.Is(err, errStreamProtocol) {
					t.Fatalf("error = %v, want a protocol error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("replyFields: %v", err)
			}
			if len(fields) != len(test.want) {
				t.Fatalf("fields = %v, want %v", fields, test.want)
			}
			for name, value := range test.want {
				if fields[name] != value {
					t.Fatalf("field %q = %v, want %v", name, fields[name], value)
				}
			}
		})
	}
}

func TestWaitForRetryStopsWhenTheConsumerIsShuttingDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !waitForRetry(ctx, time.Hour) {
		t.Fatal("waitForRetry did not report the shutdown")
	}
	if waitForRetry(context.Background(), time.Millisecond) {
		t.Fatal("waitForRetry reported a shutdown that did not happen")
	}
}

func TestEmbeddedScriptsAreLoaded(t *testing.T) {
	t.Parallel()
	if !strings.Contains(claimScript, "XAUTOCLAIM") {
		t.Fatal("the claim script does not autoclaim")
	}
	if !strings.Contains(claimScript, "deleted_pending_id") {
		t.Fatal("the claim script does not preserve deleted pending entries")
	}
	if !strings.Contains(ackDeleteScript, "XACK") || !strings.Contains(ackDeleteScript, "XDEL") {
		t.Fatal("the acknowledgement script does not acknowledge and delete")
	}
}

func TestConsumerReadsTheServersAnswersAboutItsGroup(t *testing.T) {
	t.Parallel()
	// The transport reports only that a command was rejected, so the server's
	// own text is reached through the cause. These are the answers Valkey 9
	// gives, verbatim.
	rejected := func(text string) error {
		return fmt.Errorf("read request metadata stream: %w",
			&coordination.CommandError{Cause: errors.New(text)})
	}
	cases := []struct {
		name string
		err  error
		busy bool
		lost bool
	}{
		{name: "a group that already exists is a successful creation",
			err:  rejected("BUSYGROUP: Consumer Group name already exists"),
			busy: true},
		{name: "a lost stream key leaves no group to read from",
			err: rejected("NOGROUP: No such key 'olp:i:request-metadata' or consumer " +
				"group 'olp:persistence' in XREADGROUP with GROUP option"),
			lost: true},
		{name: "the reclaim script reports the same loss through Lua",
			err: rejected("NOGROUP: No such key 'olp:i:request-metadata' or consumer " +
				"group 'olp:persistence' script: 2d8e554a, on @user_script:1."),
			lost: true},
		{name: "a missing key alone is not a missing group",
			err: rejected("An error was signalled by the server: - ResponseError: no such key")},
		{name: "a cancelled command is only worth retrying",
			err: &coordination.CommandError{Cause: context.Canceled, Ambiguous: true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isBusyGroup(test.err); got != test.busy {
				t.Fatalf("isBusyGroup = %v, want %v", got, test.busy)
			}
			run := &consumerRun{}
			if got := run.noteGroupLoss(test.err); got != test.err {
				t.Fatalf("noteGroupLoss returned %v, want the error unchanged", got)
			}
			if run.groupLost != test.lost {
				t.Fatalf("groupLost = %v, want %v", run.groupLost, test.lost)
			}
		})
	}
}
