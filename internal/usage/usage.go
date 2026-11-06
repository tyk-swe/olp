package usage

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
)

const (
	// Group is the Valkey consumer group every persistence worker joins. One
	// group across the fleet means one delivery of each event, whichever
	// process picks it up.
	Group = "olp:persistence"
	// StreamSuffix is appended to the installation's Valkey namespace to name
	// the request metadata stream.
	StreamSuffix = "request-metadata"
)

func StreamName(prefix string) string { return prefix + StreamSuffix }

const (
	// ReplayHorizonDays bounds how late a delivery may still be accounted for.
	// An event older than this cannot be added to aggregates it no longer
	// belongs to, so it is rejected and recorded as a gap instead.
	ReplayHorizonDays = 7
	// FutureSkewMinutes is the clock skew tolerated on an event observed in the
	// future; anything further ahead is rejected rather than trusted.
	FutureSkewMinutes = 5
	// ConsumerStaleAfter is the age beyond which a consumer health checkpoint
	// no longer counts as evidence that ingestion is running.
	ConsumerStaleAfter = 20 * time.Second
	// EpochStaleAfter is how long a gateway epoch may go without a checkpoint
	// before the detector treats it as a candidate for unclean shutdown.
	EpochStaleAfter = 60 * time.Second
	// EpochConfirmAfter is the second-pass delay before a stale candidate is
	// confirmed, so a paused process is not declared lost on one slow scan.
	EpochConfirmAfter = 10 * time.Second
	// ReclaimIdle is the pending-entry idle time after which another consumer
	// may claim the delivery from the one that went away.
	ReclaimIdle = 30 * time.Second
	// RecoveryInterval paces the recovery and health passes of the consumer.
	RecoveryInterval = 5 * time.Second
	// BatchSize bounds how many stream entries one read or reclaim returns.
	BatchSize = 100
)

type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Activity struct {
	Reclaimed  int64
	Recovered  int64
	Duplicates int64
	Processed  int64
}

const consumerCountersSQL = `UPDATE olp.async_worker_counters SET
        request_metadata_reclaimed_total = request_metadata_reclaimed_total + $1,
        request_metadata_recovered_total = request_metadata_recovered_total + $2,
        request_metadata_duplicates_total = request_metadata_duplicates_total + $3,
        request_metadata_processed_total = request_metadata_processed_total + $4
    WHERE singleton`

// ReportConsumerActivity folds one pass of the request metadata consumer into
// the installation counters and checkpoints the task, in a single transaction
// so the counters and the health row can never disagree about the same pass. A
// pass that did nothing writes nothing.
func ReportConsumerActivity(ctx context.Context, pool *pgxpool.Pool, region string, a Activity) error {
	if a.Reclaimed == 0 && a.Recovered == 0 && a.Duplicates == 0 && a.Processed == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("report consumer activity: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err = tx.Exec(ctx, consumerCountersSQL,
		a.Reclaimed, a.Recovered, a.Duplicates, a.Processed); err != nil {
		return fmt.Errorf("report consumer activity: %w", err)
	}
	progress := a.Reclaimed > 0 || a.Processed > 0
	if err = workerhealth.New(workerhealth.TaskRequestMetadataConsumer, region).Checkpoint(ctx, tx, workerhealth.OutcomeSuccess, progress); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("report consumer activity: %w", err)
	}
	return nil
}

// ConsumerName is this process's identity inside the consumer group. It carries
// the host and pid so an operator can find the owner of a pending delivery, and
// a per-process epoch so a restarted process never reclaims its own name (its
// abandoned entries stay pending for another consumer to claim).
func ConsumerName(hostname string) string {
	epoch := uuid.Must(uuid.NewV7())
	return consumerName(hostname, os.Getpid(), epoch)
}

func consumerName(host string, pid int, epoch uuid.UUID) string {
	label := sanitizeConsumerHost(host)
	if label == "" {
		label = "olp"
	}
	return fmt.Sprintf("%s-%d-%s", label, pid, hex.EncodeToString(epoch[:]))
}

// sanitizeConsumerHost keeps the host recognizable while guaranteeing the name is safe
// in a Valkey key, a log line and a report: lowercase alphanumerics, dashes and
// underscores survive, everything else becomes a dash, and the result is
// bounded so one long hostname cannot bloat every pending entry.
func sanitizeConsumerHost(host string) string {
	var out strings.Builder
	for _, r := range host {
		if out.Len() == 48 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r + ('a' - 'A'))
		default:
			out.WriteByte('-')
		}
	}
	return out.String()
}

// GatewayInstance distinguishes live processes even when they share a host
// or have no HOSTNAME. Restarts leave their old epochs for stale detection.
func GatewayInstance(hostname string) string {
	return consumerName(gatewayInstanceLabel(hostname), os.Getpid(), uuid.Must(uuid.NewV7()))
}

func gatewayInstanceLabel(host string) string {
	var out strings.Builder
	for _, r := range host {
		if r == '�' || unicode.IsControl(r) {
			continue
		}
		if out.Len()+len(string(r)) > 200 {
			break
		}
		out.WriteRune(r)
	}
	label := strings.TrimSpace(out.String())
	if label == "" {
		return "olp"
	}
	return label
}
