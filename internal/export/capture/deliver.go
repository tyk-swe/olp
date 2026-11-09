package capture

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/export/destination"
)

const (
	sendTimeout   = 5 * time.Second
	drainDeadline = 10 * time.Second
)

var errUnroutable = errors.New("capture policy no longer routes to a capture sink")

type route struct {
	sinkID       string
	sinkType     destination.Type
	format       destination.Format
	url          string
	settings     json.RawMessage
	credentialID *string
}

func (m *Manager) deliverLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			m.drain()
			return
		case item := <-m.queue:
			m.deliver(ctx, item)
		}
	}
}

func (m *Manager) drain() {
	deadline := time.Now().Add(drainDeadline)
	for {
		select {
		case item := <-m.queue:
			if time.Now().After(deadline) {
				m.dropped.Add(1)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), min(sendTimeout, time.Until(deadline)))
			m.deliver(ctx, item)
			cancel()
		default:
			return
		}
	}
}

func (m *Manager) deliver(ctx context.Context, item queued) {
	r, credential, err := m.route(ctx, item)
	if errors.Is(err, errUnroutable) {
		m.audit(ctx, item, "capture.dropped", "success")
		m.dropped.Add(1)
		return
	}
	if err != nil {
		m.failed.Add(1)
		m.log.Warn("capture delivery failed", "request_id", item.requestID, "code", "database")
		return
	}
	if err := m.send(ctx, r, credential, item); err != nil {
		m.audit(ctx, item, "capture.failed", "failure")
		m.failed.Add(1)
		m.log.Warn("capture delivery failed", "request_id", item.requestID, "code", destination.DeliveryErrorCode(err))
		return
	}
	m.audit(ctx, item, "capture.delivered", "success")
	m.delivered.Add(1)
}

func (m *Manager) route(ctx context.Context, item queued) (route, []byte, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return route{}, nil, err
	}
	defer tx.Rollback(ctx)
	var r route
	err = tx.QueryRow(ctx, routeSinkSQL, item.policyID, item.etag).Scan(&r.sinkID, &r.sinkType, &r.format, &r.url, &r.settings, &r.credentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return route{}, nil, errUnroutable
	}
	if err != nil {
		return route{}, nil, err
	}
	credential, err := destination.ReadCredential(ctx, tx, m.keys, m.installation, r.credentialID)
	if err != nil {
		return route{}, nil, err
	}
	return r, credential, tx.Commit(ctx)
}

func (m *Manager) send(ctx context.Context, r route, credential []byte, item queued) error {
	spec, err := destination.ParseSpec(r.sinkType, r.format, r.settings)
	if err != nil {
		return err
	}
	target, err := destination.Load(r.sinkID, r.url, spec, credential)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return m.sender.Send(ctx, target, destination.Record{
		ID: item.requestID, Stream: "captures", At: item.at, Data: item.record, ObjectKey: objectKey(item.requestID),
	})
}

func (m *Manager) audit(ctx context.Context, item queued, action, outcome string) {
	var project *string
	if item.projectID != "" {
		project = &item.projectID
	}
	if _, err := m.pool.Exec(ctx, auditSQL, access.NewID(), action, item.requestID, outcome, project); err != nil {
		m.log.Warn("capture audit failed", "request_id", item.requestID, "code", "database")
	}
}
