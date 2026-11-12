package codeforward

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/codesessions"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"

	"github.com/tyk-swe/olp/internal/upstream"
)

type codeRejectionBody struct {
	io.Reader
	io.Closer
}

func readTranslatedRejection(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	type readResult struct {
		body []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, limit))
		result <- readResult{body: data, err: err}
	}()
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case received := <-result:
		return received.body, received.err
	case <-timer.C:
		_ = body.Close()
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		_ = body.Close()
		return nil, ctx.Err()
	}
}

func (a *codeAttempt) recover(ctx context.Context, response *http.Response, route codemode.Route, previous *codesessions.Recovery, translated bool) (*codesessions.Recovery, error) {
	if a.paid != nil {
		return nil, nil
	}
	ordinal := recoveryOrdinal(previous)
	if route.InitialRecoveryAttempts < 1 || response.StatusCode != 401 && response.StatusCode != 429 || ctx.Err() != nil {
		return nil, nil
	}
	body, err := func() ([]byte, error) {
		if translated {
			return readTranslatedRejection(ctx, response.Body, (64<<10)+1)
		}
		return io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	}()
	response.Body = codeRejectionBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), Closer: response.Body}
	if err != nil || len(body) > 64<<10 {
		return nil, nil
	}
	adapter, ok := codeadapter.ForConnection(a.config.Kind, a.config.AuthMode, a.config.ProfileID)
	rejection, proven := adapter.InitialRejection(a.endpoint, response.StatusCode, body, response.Header, a.forwarder.now())
	if !ok || !proven {
		return nil, nil
	}
	if err := a.forwarder.ledger.ConfirmNonexecution(ctx, a.permit.Attempt.ID, response.StatusCode); err != nil {
		return nil, nil
	}
	rejection = codeadapter.RejectionWithCycleReset(rejection, a.permit.Account.CycleAnchor, a.forwarder.now().UTC())
	zero := int64(0)
	a.usage, a.terminal = codemode.Usage{Total: &zero}, true
	recorded := func() bool {
		if rejection.Class != upstream.RateLimit {
			return true
		}
		_, ok := a.observeCooldownClass(codemode.Observation{Status: response.StatusCode, RejectionBody: body, RetryAfter: response.Header.Values("Retry-After")})
		if ok {
			a.pendingHealth = nil
		}
		return ok
	}()
	a.finish(ctx)
	if !recorded {
		return nil, nil
	}
	if rejection.Class == upstream.Credential {
		if err := a.forwarder.ledger.ObserveCooldown(ctx, a.permit.Account.ID, rejection.RetryAt); err != nil {
			return nil, nil
		}
	}
	if !a.permit.InitialRecovery {
		return nil, codemode.Refuse(409, "code_recovery_unsafe")
	}
	if ordinal > route.InitialRecoveryAttempts {
		return nil, nil
	}
	if rejection.Class == upstream.Credential && a.auth.CredentialID != "" && a.auth.GrantGeneration > 0 && a.refreshed(ctx, adapter) {
		return &codesessions.Recovery{From: a.permit.Attempt.ID, Envelope: rejection.Envelope, Number: ordinal, AccountID: a.permit.Account.ID, CredentialID: a.auth.CredentialID, GrantGeneration: a.auth.GrantGeneration, RejectedUntil: rejection.RetryAt}, nil
	}
	return &codesessions.Recovery{From: a.permit.Attempt.ID, Envelope: rejection.Envelope, Number: ordinal}, nil
}

func (a *codeAttempt) refreshed(ctx context.Context, adapter codeadapter.Vendor) bool {
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		auth, err := a.forwarder.authorizer.AuthorizeCode(refreshCtx, a.config, a.permit.Account, codemode.Dispatch{Adapter: adapter.Adapter, Protocol: a.endpoint.Protocol, ClientPath: a.endpoint.ClientPath})
		if err == nil && auth.CredentialID == a.auth.CredentialID && auth.GrantGeneration > a.auth.GrantGeneration && adapter.ValidateAuthorization(a.endpoint, auth, a.permit.Pin.Principal) == nil {
			return true
		}
		select {
		case <-refreshCtx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func recoveryOrdinal(previous *codesessions.Recovery) int {
	if previous != nil {
		return previous.Number + 1
	}
	return 1
}
