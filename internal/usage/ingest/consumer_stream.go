package ingest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

func (r *consumerRun) createGroup(ctx context.Context) error {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XGROUP", "CREATE", r.stream, consumerGroup, "0", "MKSTREAM")
	if err != nil {
		if isBusyGroup(err) {
			return nil
		}
		return fmt.Errorf("create request metadata consumer group: %w", err)
	}
	if text, ok := reply.(string); !ok || text != "OK" {
		return protocolError("invalid consumer group creation reply")
	}
	return nil
}

// serverAnswer is the server's own text for a failed command. The transport
// keeps that text out of its message so logs cannot leak command arguments, so
// the cause is read instead.
func serverAnswer(err error) string {
	var command *coordination.CommandError
	if errors.As(err, &command) && command.Cause != nil {
		err = command.Cause
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func isBusyGroup(err error) bool { return strings.Contains(serverAnswer(err), "BUSYGROUP") }

// noteGroupLoss records a stream command that found no consumer group and
// returns the error unchanged, so every caller keeps reporting it as before.
// Valkey answers NOGROUP to every group command once the stream key is gone,
// including after a later write recreates the key without the group, so the
// loop has to create it again rather than retry the read forever.
func (r *consumerRun) noteGroupLoss(err error) error {
	if strings.Contains(serverAnswer(err), "NOGROUP") {
		r.groupLost = true
	}
	return err
}

// readGroup reads one page of deliveries. `id` is ">" for new work or a pending
// cursor for replay; a positive block waits server side for new entries.
func (r *consumerRun) readGroup(ctx context.Context, id string, block time.Duration) ([]StreamEntry, error) {
	args := []string{"XREADGROUP", "GROUP", consumerGroup, r.consumer, "COUNT", strconv.Itoa(r.policy.batchSize)}
	timeout := consumerCommandTimeout
	if block > 0 {
		args = append(args, "BLOCK", strconv.FormatInt(block.Milliseconds(), 10))
		timeout = block + consumerResponseMargin
	}
	args = append(args, "STREAMS", r.stream, id)
	command, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	do := r.client.Do
	if block > 0 {
		do = r.client.DoBlocking
	}
	reply, err := do(command, args...)
	if err != nil {
		return nil, r.noteGroupLoss(fmt.Errorf("read request metadata stream: %w", err))
	}
	return parseReadReply(reply, r.stream, r.policy.batchSize)
}

func (r *consumerRun) autoClaim(ctx context.Context, start string) (*autoClaimPage, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "EVAL", claimScript, "1", r.stream, consumerGroup, r.consumer,
		strconv.FormatInt(r.policy.reclaimIdle.Milliseconds(), 10), start,
		strconv.Itoa(r.policy.batchSize))
	if err != nil {
		return nil, r.noteGroupLoss(fmt.Errorf("reclaim request metadata deliveries: %w", err))
	}
	return parseAutoClaimReply(reply, r.policy.batchSize)
}

// acknowledge retires one delivery after its transaction committed. It runs on
// a context that outlives shutdown: the work is already durable, and leaving
// the entry pending would only earn a duplicate delivery later.
func (r *consumerRun) acknowledge(ctx context.Context, id string) error {
	command, cancel := context.WithTimeout(context.WithoutCancel(ctx), consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "EVAL", ackDeleteScript, "1", r.stream, consumerGroup, id)
	if err != nil {
		return fmt.Errorf("acknowledge request metadata delivery: %w", err)
	}
	counts, ok := reply.([]any)
	if !ok || len(counts) != 2 {
		return protocolError("invalid stream acknowledgement reply")
	}
	for _, count := range counts {
		value, ok := count.(int64)
		if !ok || value < 0 || value > 1 {
			return protocolError("invalid stream acknowledgement reply")
		}
	}
	return nil
}

func (r *consumerRun) checkpointHealth(ctx context.Context) error {
	pending, oldest, err := r.pendingSummary(ctx)
	if err != nil {
		return err
	}
	lag, oldestLagged, err := r.groupLag(ctx, pending)
	if err != nil {
		return err
	}
	return completeness.ReportConsumerHealth(ctx, r.pool, r.region, pending, lag, oldest, oldestLagged, time.Now().UTC())
}

func (r *consumerRun) pendingSummary(ctx context.Context) (int64, *time.Time, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XPENDING", r.stream, consumerGroup)
	if err != nil {
		return 0, nil, r.noteGroupLoss(fmt.Errorf("read request metadata pending summary: %w", err))
	}
	items, ok := reply.([]any)
	if !ok || len(items) != 4 {
		return 0, nil, protocolError("unrecognized pending stream reply")
	}
	count, ok := items[0].(int64)
	if !ok || count < 0 {
		return 0, nil, protocolError("unrecognized pending stream reply")
	}
	if count == 0 {
		return 0, nil, nil
	}
	start, ok := items[1].(string)
	if !ok {
		return 0, nil, protocolError("unrecognized pending stream reply")
	}
	milliseconds, _, err := ParseStreamID(start)
	if err != nil {
		return 0, nil, err
	}
	if milliseconds > 1<<62 {
		return 0, nil, protocolError("pending stream ID overflow")
	}
	oldest := time.UnixMilli(int64(milliseconds)).UTC()
	return count, &oldest, nil
}

// groupLag is how many entries the group has never been delivered, and when
// the oldest of them was written. Undelivered entries are always the tail of
// the stream, so the lag newest entries end at the oldest lagged one. Valkey
// may return an unknown lag while deliveries and deletions race; because
// every acknowledged entry is deleted in the same step, the stream's
// remaining length minus what is pending is a safe fallback.
func (r *consumerRun) groupLag(ctx context.Context, pending int64) (int64, *time.Time, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XINFO", "GROUPS", r.stream)
	if err != nil {
		return 0, nil, fmt.Errorf("read request metadata group info: %w", err)
	}
	groups, ok := reply.([]any)
	if !ok {
		return 0, nil, protocolError("invalid XINFO GROUPS reply")
	}
	var lag int64
	for _, item := range groups {
		fields, err := replyFields(item)
		if err != nil {
			return 0, nil, err
		}
		if name, ok := fields["name"].(string); !ok || name != consumerGroup {
			continue
		}
		if reported, ok := fields["lag"].(int64); ok {
			lag = max(reported, 0)
		} else {
			groupPending, ok := fields["pending"].(int64)
			if !ok {
				groupPending = pending
			}
			length, err := r.streamLength(ctx)
			if err != nil {
				return 0, nil, err
			}
			lag = max(length-groupPending, 0)
		}
		if lag == 0 {
			return 0, nil, nil
		}
		oldest, err := r.oldestLaggedEntry(ctx, lag)
		if err != nil {
			return 0, nil, err
		}
		return lag, oldest, nil
	}
	return 0, nil, protocolError("consumer group disappeared")
}

// oldestLaggedEntry reads back the lag newest entries — exactly the ones the
// group has never been delivered — and returns when the earliest of them was
// written. A reported lag whose entries are already gone cannot age, so the
// sample fails rather than publish one.
func (r *consumerRun) oldestLaggedEntry(ctx context.Context, lag int64) (*time.Time, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XREVRANGE", r.stream, "+", "-", "COUNT", strconv.FormatInt(lag, 10))
	if err != nil {
		return nil, fmt.Errorf("read request metadata lagged entries: %w", err)
	}
	entries, ok := reply.([]any)
	if !ok {
		return nil, protocolError("invalid lagged entries reply")
	}
	if len(entries) == 0 {
		return nil, protocolError("reported consumer lag has no entries")
	}
	oldest, ok := entries[len(entries)-1].([]any)
	if !ok || len(oldest) == 0 {
		return nil, protocolError("invalid lagged entry reply")
	}
	id, ok := oldest[0].(string)
	if !ok {
		return nil, protocolError("invalid lagged entry id")
	}
	milliseconds, _, err := ParseStreamID(id)
	if err != nil {
		return nil, err
	}
	if milliseconds > 1<<62 {
		return nil, protocolError("lagged stream ID overflow")
	}
	written := time.UnixMilli(int64(milliseconds)).UTC()
	return &written, nil
}

func (r *consumerRun) streamLength(ctx context.Context) (int64, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XLEN", r.stream)
	if err != nil {
		return 0, fmt.Errorf("read request metadata stream length: %w", err)
	}
	length, ok := reply.(int64)
	if !ok || length < 0 {
		return 0, protocolError("invalid XLEN reply")
	}
	return length, nil
}

// replyFields reads a reply that is a map under RESP3 and a flat field list
// otherwise.
func replyFields(reply any) (map[string]any, error) {
	switch value := reply.(type) {
	case map[string]any:
		return value, nil
	case []any:
		if len(value)%2 != 0 {
			return nil, protocolError("field list has odd length")
		}
		fields := make(map[string]any, len(value)/2)
		for index := 0; index+1 < len(value); index += 2 {
			name, ok := value[index].(string)
			if !ok {
				return nil, protocolError("field list has a non-string name")
			}
			fields[name] = value[index+1]
		}
		return fields, nil
	default:
		return nil, protocolError("invalid field container")
	}
}
