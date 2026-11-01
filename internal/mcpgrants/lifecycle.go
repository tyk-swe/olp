package mcpgrants

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Flow string

const (
	AuthorizationCode Flow = "authorization_code"
	DeviceCode        Flow = "device_code"
)

type DeviceChallenge struct {
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	Interval        int64     `json:"interval"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type Authorization struct {
	State            StandingState    `json:"state"`
	AuthorizationURL string           `json:"authorization_url,omitempty"`
	Device           *DeviceChallenge `json:"device,omitempty"`
}

type flowSession struct {
	Verifier   string `json:"verifier,omitempty"`
	State      string `json:"state,omitempty"`
	Redirect   string `json:"redirect,omitempty"`
	DeviceCode string `json:"device_code,omitempty"`
}

type flowRecord struct {
	Enrollment
	Flow      Flow
	SessionID *string
	AttemptID *string
	Interval  int64
	Outcome   string
}

func readFlow(ctx context.Context, q access.Queryer, id, member string) (flowRecord, error) {
	var item flowRecord
	err := q.QueryRow(ctx, `SELECT id::text,binding,expires_at,claimed_by::text,coalesce(flow,''),session_secret_id::text,exchange_attempt_id::text,coalesce(poll_interval,0),coalesce(outcome,'') FROM olp.mcp_grant_enrollments WHERE id=$1 AND claimed_by=$2 AND expires_at>now()`, id, member).Scan(&item.ID, &item.Binding, &item.ExpiresAt, &item.ClaimedBy, &item.Flow, &item.SessionID, &item.AttemptID, &item.Interval, &item.Outcome)
	return item, err
}

func (s *Store) Authorize(ctx context.Context, id, member string, flow Flow, redirect string) (Authorization, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Authorization{}, err
	}
	defer tx.Rollback(ctx)
	item, readErr := readFlow(ctx, tx, id, member)
	if readErr != nil {
		return Authorization{}, readErr
	}
	if item.Binding.OAuth.EntraOBO != nil {
		return Authorization{}, ErrRequired
	}
	if item.Flow != "" || item.Outcome != "" {
		return Authorization{}, access.Fail(409, "grant_enrollment_used", "This enrollment already started authorization.")
	}
	if _, bindingErr := liveBinding(ctx, tx, item.Binding); bindingErr != nil {
		return Authorization{}, bindingErr
	}
	client, clientErr := s.oauthClient(ctx, item.Binding, redirect, "")
	if clientErr != nil {
		return Authorization{}, clientErr
	}
	defer client.client.CloseIdleConnections()
	if flow != AuthorizationCode && flow != DeviceCode {
		return Authorization{}, access.Invalid("flow", "Use authorization_code or device_code.")
	}
	if flow == DeviceCode && item.Binding.OAuth.DeviceAuthorizationEndpoint == "" {
		return Authorization{}, access.Invalid("flow", "This server has no declared device authorization endpoint.")
	}
	session := flowSession{Verifier: oauth2.GenerateVerifier(), State: id + "." + oauth2.GenerateVerifier(), Redirect: redirect}
	secretID, sealErr := s.sealFlow(ctx, tx, item, flow, session)
	if sealErr != nil {
		return Authorization{}, sealErr
	}
	changed, updateErr := tx.Exec(ctx, `UPDATE olp.mcp_grant_enrollments SET flow=$2,session_secret_id=$3 WHERE id=$1 AND flow IS NULL`, id, flow, secretID)
	if updateErr != nil {
		return Authorization{}, updateErr
	}
	if changed.RowsAffected() != 1 {
		return Authorization{}, access.Fail(409, "grant_enrollment_used", "This enrollment already started authorization.")
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return Authorization{}, commitErr
	}
	if flow == AuthorizationCode {
		return Authorization{State: Pending, AuthorizationURL: client.config.AuthCodeURL(session.State, oauth2.S256ChallengeOption(session.Verifier), oauth2.SetAuthURLParam("audience", item.Binding.OAuth.Audience))}, nil
	}
	device, deviceErr := client.config.DeviceAuth(client.context(ctx), oauth2.SetAuthURLParam("audience", item.Binding.OAuth.Audience))
	if deviceErr != nil {
		return Authorization{}, errors.Join(errOAuth, s.endFlow(ctx, id, "failed"))
	}
	if device.DeviceCode == "" || len(device.DeviceCode) > 16384 || device.UserCode == "" || len(device.UserCode) > 64 || strings.ContainsAny(device.UserCode, "\r\n\x00") || !device.Expiry.After(time.Now()) || device.Interval < 0 || device.Interval > 300 {
		return Authorization{}, errors.Join(errOAuth, s.endFlow(ctx, id, "failed"))
	}
	if verifyErr := s.verificationURL(item.Binding, device.VerificationURI); verifyErr != nil {
		return Authorization{}, errors.Join(verifyErr, s.endFlow(ctx, id, "failed"))
	}
	interval := grants.DevicePollInterval(time.Duration(device.Interval)*time.Second, false)
	expires := minTime(item.ExpiresAt, minTime(device.Expiry, time.Now().Add(grants.MaxDeviceTTL)))
	if saveErr := s.saveDevice(ctx, item, device.DeviceCode, expires, interval); saveErr != nil {
		return Authorization{}, saveErr
	}
	return Authorization{State: Pending, Device: &DeviceChallenge{VerificationURL: device.VerificationURI, UserCode: device.UserCode, Interval: int64(interval / time.Second), ExpiresAt: expires}}, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Store) saveDevice(ctx context.Context, item flowRecord, deviceCode string, expires time.Time, interval time.Duration) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := liveBinding(ctx, tx, item.Binding); err != nil {
		return err
	}
	session, encodeErr := json.Marshal(flowSession{DeviceCode: deviceCode})
	if encodeErr != nil {
		return encodeErr
	}
	defer clear(session)
	id := access.NewID()
	if err := s.Keys.Store(ctx, tx, s.Installation, id, secrets.MCPGrantEnrollment, session, &expires); err != nil {
		return err
	}
	tag, updateErr := tx.Exec(ctx, `UPDATE olp.mcp_grant_enrollments SET session_secret_id=$2,expires_at=$3,poll_interval=$4,poll_at=now() WHERE id=$1 AND flow='device_code' AND session_secret_id IS NULL AND outcome IS NULL AND expires_at>now()`, item.ID, id, expires, int64(interval/time.Second))
	if updateErr != nil {
		return updateErr
	}
	if tag.RowsAffected() != 1 {
		return ErrRequired
	}
	return tx.Commit(ctx)
}

func (s *Store) loadSession(ctx context.Context, q access.Queryer, item flowRecord) (flowSession, error) {
	if item.SessionID == nil {
		return flowSession{}, errOAuth
	}
	encoded, err := s.Keys.Read(ctx, q, s.Installation, *item.SessionID, secrets.MCPGrantEnrollment)
	if err != nil {
		return flowSession{}, err
	}
	defer clear(encoded)
	var session flowSession
	decodeErr := json.Unmarshal(encoded, &session)
	return session, decodeErr
}

func (s *Store) Complete(ctx context.Context, id, member, code, state string) (Authorization, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Authorization{}, err
	}
	defer tx.Rollback(ctx)
	item, readErr := readFlow(ctx, tx, id, member)
	if readErr != nil {
		return Authorization{}, readErr
	}
	if item.Flow != AuthorizationCode || item.AttemptID != nil || item.Outcome != "" {
		return Authorization{}, access.Fail(409, "grant_enrollment_used", "This enrollment cannot exchange another code.")
	}
	session, sessionErr := s.loadSession(ctx, tx, item)
	if sessionErr != nil {
		return Authorization{}, sessionErr
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(session.State)) != 1 {
		return Authorization{}, access.Invalid("state", "This callback belongs to another authorization.")
	}
	attempt := access.NewID()
	tag, claimErr := tx.Exec(ctx, `UPDATE olp.mcp_grant_enrollments SET exchange_attempt_id=$2 WHERE id=$1 AND exchange_attempt_id IS NULL AND outcome IS NULL`, id, attempt)
	if claimErr != nil {
		return Authorization{}, claimErr
	}
	if tag.RowsAffected() != 1 {
		return Authorization{}, access.Fail(409, "grant_enrollment_used", "This authorization code was already exchanged.")
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return Authorization{}, commitErr
	}
	client, clientErr := s.oauthClient(ctx, item.Binding, session.Redirect, "")
	if clientErr != nil {
		return Authorization{}, clientErr
	}
	defer client.client.CloseIdleConnections()
	token, exchangeErr := client.config.Exchange(client.context(ctx), code, oauth2.VerifierOption(session.Verifier))
	if exchangeErr != nil {
		return Authorization{}, errors.Join(errOAuth, s.endFlow(ctx, id, "failed"))
	}
	if storeErr := s.completeGrant(ctx, item, attempt, token); storeErr != nil {
		return Authorization{}, storeErr
	}
	return Authorization{State: Active}, nil
}

func (s *Store) endFlow(ctx context.Context, id, outcome string) error {
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.Pool.Exec(record, `UPDATE olp.mcp_grant_enrollments SET outcome=$2 WHERE id=$1 AND outcome IS NULL`, id, outcome)
	return err
}

func (s *Store) completeGrant(ctx context.Context, item flowRecord, attempt string, token *oauth2.Token) error {
	if err := validToken(token); err != nil {
		return errors.Join(err, s.endFlow(ctx, item.ID, "failed"))
	}
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := s.Pool.Begin(record)
	if err != nil {
		return err
	}
	defer tx.Rollback(record)
	if _, err := liveBinding(record, tx, item.Binding); err != nil {
		return err
	}
	tag, finishErr := tx.Exec(record, `UPDATE olp.mcp_grant_enrollments SET outcome='completed' WHERE id=$1 AND exchange_attempt_id=$2 AND outcome IS NULL AND expires_at>now()`, item.ID, attempt)
	if finishErr != nil {
		return finishErr
	}
	if tag.RowsAffected() != 1 {
		return ErrRequired
	}
	accessID := access.NewID()
	if err := s.Keys.Store(record, tx, s.Installation, accessID, item.Binding.accessPurpose(), []byte(token.AccessToken), nil); err != nil {
		return err
	}
	refreshID, refreshErr := s.storeRefresh(record, tx, item.Binding, token.RefreshToken)
	if refreshErr != nil {
		return refreshErr
	}
	_, insertErr := tx.Exec(record, `INSERT INTO olp.mcp_user_grants(id,enrollment_id,server_id,agent_id,project_id,binding,binding_digest,member_id,access_secret_id,refresh_secret_id,expires_at) VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(binding_digest) DO UPDATE SET id=excluded.id,enrollment_id=excluded.enrollment_id,member_id=excluded.member_id,access_secret_id=excluded.access_secret_id,refresh_secret_id=excluded.refresh_secret_id,expires_at=excluded.expires_at,revoked_at=NULL,refresh_attempt_id=NULL,failure_code=NULL`, access.NewID(), item.ID, item.Binding.ServerID, item.Binding.AgentID, item.Binding.ProjectID, item.Binding, item.Binding.Digest(), item.ClaimedBy, accessID, refreshID, token.Expiry)
	if insertErr != nil {
		return insertErr
	}
	if item.SessionID != nil {
		if _, err := tx.Exec(record, `UPDATE olp.mcp_grant_enrollments SET session_secret_id=NULL WHERE id=$1`, item.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(record, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, *item.SessionID, secrets.MCPGrantEnrollment); err != nil {
			return err
		}
	}
	return tx.Commit(record)
}

func (s *Store) storeRefresh(ctx context.Context, tx pgx.Tx, binding Binding, token string) (*string, error) {
	if token == "" {
		return nil, nil
	}
	id := access.NewID()
	if err := s.Keys.Store(ctx, tx, s.Installation, id, binding.refreshPurpose(), []byte(token), nil); err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Store) sealFlow(ctx context.Context, tx pgx.Tx, item flowRecord, flow Flow, session flowSession) (*string, error) {
	if flow != AuthorizationCode {
		return nil, nil
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	defer clear(encoded)
	id := access.NewID()
	if err := s.Keys.Store(ctx, tx, s.Installation, id, secrets.MCPGrantEnrollment, encoded, &item.ExpiresAt); err != nil {
		return nil, err
	}
	return &id, nil
}
