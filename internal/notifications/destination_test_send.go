package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const (
	testClaimLifetime = 30 * time.Second
	testSettleTimeout = 5 * time.Second
	testsPerMinute    = 3
	testAuditResource = "notification_destination_test"
)

type testKey struct{ destination, action string }

type testResult struct {
	ID      string                          `json:"id"`
	Status  contract.NotificationTestStatus `json:"status"`
	Failure *Failure                        `json:"failure,omitempty"`
}

type testClaim struct {
	result    testResult
	projectID *string
	target    destination
	secret    []byte
	expires   time.Time
	ownsSend  bool
}

func destinationTestKey(r *http.Request) (testKey, error) {
	id, err := httpkit.IDParam(r, destinations.parameter)
	if err != nil {
		return testKey{}, err
	}
	action, err := httpkit.IDParam(r, "action_id")
	return testKey{destination: id, action: action}, err
}

func (s *Server) testStatus(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	key, err := destinationTestKey(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var projectID *string
	if err := s.access.Pool.QueryRow(r.Context(), testScopeSQL, key.destination).Scan(&projectID); err != nil {
		return httpkit.Reply{}, err
	}
	if err := authorizeChange(p, projectID); err != nil {
		return httpkit.Reply{}, err
	}
	result, err := readTest(r.Context(), s.access.Pool, key)
	return httpkit.OK(result), err
}

func (s *Server) sendTest(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	key, err := destinationTestKey(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	claim, err := access.Run(s.access, r, func(tx pgx.Tx, p authz.Principal) (testClaim, access.Event, error) {
		locked, err := destinations.lockForChange(r.Context(), tx, p, key.destination)
		if err != nil {
			return testClaim{}, access.Event{}, err
		}
		claim, err := s.claimTest(r.Context(), tx, key, locked.projectID)
		if err != nil || !claim.ownsSend {
			return claim, access.Event{}, err
		}
		return claim, access.Event{
			Action: "notification_destination_test.claim", Resource: testAuditResource, ID: claim.result.ID, Project: locked.projectID,
		}, nil
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !claim.ownsSend {
		return httpkit.OK(claim.result), nil
	}
	return s.attemptTest(r, key, claim)
}

func (s *Server) claimTest(ctx context.Context, tx pgx.Tx, key testKey, projectID *string) (testClaim, error) {
	existing, err := readTest(ctx, tx, key)
	if !errors.Is(err, pgx.ErrNoRows) {
		return testClaim{result: existing}, err
	}
	target, secret, err := s.testTarget(ctx, tx, key.destination)
	if err != nil {
		return testClaim{}, err
	}
	id := httpkit.NewID()
	var expires time.Time
	err = tx.QueryRow(ctx, insertTestSQL, id, key.destination, key.action, int(testClaimLifetime/time.Second), testsPerMinute).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return testClaim{}, httpkit.Fail(http.StatusTooManyRequests, "notification_test_rate_limit", "Wait a minute before starting another test on this destination.")
	}
	if err != nil {
		return testClaim{}, err
	}
	return testClaim{
		result:    testResult{ID: id, Status: contract.NotificationTestStatusPending},
		projectID: projectID, target: target, secret: secret, expires: expires, ownsSend: true,
	}, nil
}

func (s *Server) testTarget(ctx context.Context, tx pgx.Tx, id string) (destination, []byte, error) {
	var target destination
	var enabled bool
	var secretID *string
	if err := tx.QueryRow(ctx, testTargetSQL, id).Scan(&target.URL, &target.Type, &target.Configuration, &enabled, &secretID); err != nil {
		return destination{}, nil, err
	}
	if !enabled || (target.Type != contract.NotificationDestinationTypeWebhook && target.Type != contract.NotificationDestinationTypeEmail) {
		return destination{}, nil, httpkit.Invalid("destination", "Only enabled webhook and email destinations can send a test.")
	}
	if secretID == nil {
		return target, nil, nil
	}
	secret, err := s.access.Keys.Read(ctx, tx, s.access.Installation, *secretID, secrets.NotificationSecret)
	return target, secret, err
}

func (s *Server) attemptTest(r *http.Request, key testKey, claim testClaim) (httpkit.Reply, error) {
	ctx := context.WithoutCancel(r.Context())
	sendCtx, stop := context.WithDeadline(ctx, claim.expires.Add(-testSettleTimeout))
	defer stop()
	code := s.sendTestMessage(sendCtx, claim)
	status, failure := testOutcome(code)

	settle, cancel := context.WithTimeout(ctx, testSettleTimeout)
	defer cancel()
	result, err := access.Run(s.access, r.WithContext(settle), func(tx pgx.Tx, _ authz.Principal) (testResult, access.Event, error) {
		finished, err := tx.Exec(settle, finishTestSQL, claim.result.ID, status, failure)
		if err != nil {
			return testResult{}, access.Event{}, err
		}
		current, err := readTest(settle, tx, key)
		if finished.RowsAffected() == 0 {
			return current, access.Event{}, err
		}
		return current, access.Event{
			Action: "notification_destination_test." + string(status), Resource: testAuditResource,
			ID: claim.result.ID, Project: claim.projectID, Failed: code != "",
		}, err
	})
	return httpkit.OK(result), err
}

func (s *Server) sendTestMessage(ctx context.Context, claim testClaim) Failure {
	if ctx.Err() != nil {
		return timeoutCode
	}
	const body = `{"event":"notification.test","test":true,"message":"This is an OLP test notification."}`
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return invalidDestinationCode
	}
	m := message{title: "OLP test notification", body: []byte(body), fields: fields, date: time.Now()}
	return sendErrorCode(s.sender.send(ctx, claim.target, claim.secret, m))
}

func testOutcome(code Failure) (contract.NotificationTestStatus, *Failure) {
	switch code {
	case "":
		return contract.NotificationTestStatusDelivered, nil
	case timeoutCode, networkCode:
		return contract.NotificationTestStatusUnconfirmed, &code
	default:
		return contract.NotificationTestStatusFailed, &code
	}
}

func readTest(ctx context.Context, db database.Queryer, key testKey) (testResult, error) {
	var result testResult
	err := db.QueryRow(ctx, readTestSQL, key.destination, key.action).Scan(&result.ID, &result.Status, &result.Failure)
	return result, err
}

const (
	testScopeSQL  = `SELECT project_id::text FROM olp.notification_destinations WHERE id=$1`
	testTargetSQL = `SELECT url,type,configuration,enabled,secret_id::text FROM olp.notification_destinations WHERE id=$1`
	readTestSQL   = `SELECT id::text,CASE WHEN finished_at IS NULL AND expires_at>clock_timestamp() THEN 'pending' ELSE status END,failure
 FROM olp.notification_destination_tests WHERE destination_id=$1 AND action_id=$2`
	insertTestSQL = `INSERT INTO olp.notification_destination_tests(id,destination_id,action_id,claimed_at,expires_at)
 SELECT $1,$2,$3,stamp,stamp+make_interval(secs=>$4::int) FROM (SELECT clock_timestamp() AS stamp) AS claim
 WHERE (SELECT count(*) FROM olp.notification_destination_tests WHERE destination_id=$2 AND claimed_at>stamp-interval '1 minute')<$5
 RETURNING expires_at`
	finishTestSQL = `UPDATE olp.notification_destination_tests SET status=$2,failure=$3,finished_at=clock_timestamp()
 WHERE id=$1 AND finished_at IS NULL AND expires_at>clock_timestamp()`
)
