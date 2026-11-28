package identity

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type mfaProof struct {
	Challenge  string          `json:"challenge"`
	Method     string          `json:"method"`
	Code       string          `json:"code"`
	Credential json.RawMessage `json:"credential"`
}

type mfaVerification struct {
	valid      bool
	credential *webauthn.Credential
	counter    int64
}

type mfaEnrolledResponse struct {
	Enrolled      bool     `json:"enrolled"`
	RecoveryCodes []string `json:"recovery_codes"`
}

type mfaBootstrapEnrolledResponse struct {
	Session       any      `json:"session"`
	RecoveryCodes []string `json:"recovery_codes"`
}

// totpCounter returns a fresh RFC 6238 counter within one 30-second step of
// the server clock. A caller atomically persists it to prevent cross-gateway replay.
func totpCounter(secret, code string, now time.Time, last int64) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	current := now.Unix() / 30
	for counter := current - 1; counter <= current+1; counter++ {
		if counter < 0 || counter <= last {
			continue
		}
		expected, err := hotp.GenerateCodeCustom(secret, uint64(counter), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return counter, true
		}
	}
	return 0, false
}

func (s *mfaService) verifyMFAProof(r *http.Request, tx pgx.Tx, f mfaFlow, input mfaProof) (bool, error) {
	switch input.Method {
	case "totp":
		return s.verifyTOTPProof(r.Context(), tx, f.UserID, input.Code)
	case "recovery":
		return s.verifyRecoveryProof(r.Context(), tx, f.UserID, input.Code)
	case "webauthn":
		return s.verifyWebAuthnProof(r.Context(), tx, f, input.Credential)
	default:
		return false, nil
	}
}

func (s *mfaService) verifyTOTPProof(ctx context.Context, tx pgx.Tx, userID, code string) (bool, error) {
	var id string
	var last int64
	err := tx.QueryRow(ctx, "SELECT id::text,last_counter FROM olp.mfa_factors WHERE user_id=$1 AND kind='totp' FOR UPDATE", userID).Scan(&id, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	secret, err := s.access.Keys.Read(ctx, tx, s.access.Installation, id, secrets.MFATOTP)
	if err != nil {
		return false, err
	}
	counter, valid := totpCounter(string(secret), code, time.Now(), last)
	clear(secret)
	if !valid {
		return false, nil
	}
	_, err = tx.Exec(ctx, "UPDATE olp.mfa_factors SET last_counter=$2,last_used_at=now() WHERE id=$1", id, counter)
	return err == nil, err
}

func (s *mfaService) verifyRecoveryProof(ctx context.Context, tx pgx.Tx, userID, rawCode string) (bool, error) {
	code, ok := normalizeRecoveryCode(rawCode)
	if !ok {
		return false, nil
	}
	deleted, err := tx.Exec(ctx, "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1 AND digest=$2", userID, s.access.Auth.Digest(secrets.MFARecoveryDigest, userID+":"+code))
	return err == nil && deleted.RowsAffected() == 1, err
}

func (s *mfaService) verifyWebAuthnProof(ctx context.Context, tx pgx.Tx, f mfaFlow, rawCredential json.RawMessage) (bool, error) {
	if f.Data.WebAuthn == nil || len(rawCredential) == 0 {
		return false, nil
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(rawCredential)
	if err != nil {
		return false, nil
	}
	user, err := s.mfaUser(ctx, tx, f.UserID)
	if err != nil {
		return false, err
	}
	w, err := s.webAuthn()
	if err != nil {
		return false, err
	}
	credential, err := w.ValidateLogin(user, *f.Data.WebAuthn, parsed)
	if err != nil || credential.Authenticator.CloneWarning {
		return false, nil
	}
	var id string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.mfa_factors WHERE user_id=$1 AND credential_id=$2 FOR UPDATE", f.UserID, credential.ID).Scan(&id); err != nil {
		return false, err
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return false, err
	}
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.MFAWebAuthn, raw, nil); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, "UPDATE olp.mfa_factors SET last_used_at=now() WHERE id=$1", id)
	return err == nil, err
}

func (s *mfaService) recordMFAFailure(r *http.Request, tx pgx.Tx, f mfaFlow) (httpkit.Reply, bool, error) {
	if _, err := tx.Exec(r.Context(), "UPDATE olp.mfa_challenges SET attempts=attempts+1 WHERE id=$1 AND attempts<5", f.ID); err != nil {
		return httpkit.Reply{}, false, err
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(f.UserID), "mfa.verify", "user", f.UserID, "failure"); err != nil {
		return httpkit.Reply{}, false, err
	}
	return httpkit.Reply{}, true, problem.Fail(401, "mfa_verification_failed", "The verification did not succeed. Try another current factor or recovery code.")
}

func (s *mfaService) mfaVerify(r *http.Request) (httpkit.Reply, error) {
	var input mfaProof
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if err := admitAuth(s.access, r, "mfa_request", ""); err != nil {
		return httpkit.Reply{}, err
	}
	preflight, err := s.readMFAFlow(r, s.access.Pool, input.Challenge, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := admitAuth(s.access, r, "mfa_verify", preflight.UserID); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		return s.finishMFAVerify(r, tx, input)
	})
}

func (s *mfaService) finishMFAVerify(r *http.Request, tx pgx.Tx, input mfaProof) (httpkit.Reply, bool, error) {
	f, err := s.loadMFAFlow(r, tx, input.Challenge)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	result, err := s.evaluateMFAFlow(r, tx, f, input)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	if !result.valid {
		return s.recordMFAFailure(r, tx, f)
	}
	var codes []string
	if strings.HasPrefix(f.Purpose, "enroll_") {
		codes, err = s.activateMFA(r, tx, f, result.credential, result.counter)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.mfa_challenges WHERE id=$1", f.ID); err != nil {
		return httpkit.Reply{}, false, err
	}
	reply, err := s.completeMFAOutcome(r, tx, f, codes)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(f.UserID), "mfa.verify", "user", f.UserID, "success"); err != nil {
		return httpkit.Reply{}, false, err
	}
	return reply, true, nil
}

func (s *mfaService) evaluateMFAFlow(r *http.Request, tx pgx.Tx, f mfaFlow, input mfaProof) (mfaVerification, error) {
	switch f.Purpose {
	case "login", "manage", "reauth":
		valid, err := s.verifyMFAProof(r, tx, f, input)
		return mfaVerification{valid: valid}, err
	case "enroll_totp":
		return s.verifyTOTPEnrollment(r.Context(), tx, f, input)
	case "enroll_webauthn":
		return s.verifyWebAuthnEnrollment(r.Context(), tx, f, input)
	default:
		return mfaVerification{}, problem.Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
}

func (s *mfaService) verifyTOTPEnrollment(ctx context.Context, tx pgx.Tx, f mfaFlow, input mfaProof) (mfaVerification, error) {
	if input.Method != "totp" || f.SecretID == nil {
		return mfaVerification{}, nil
	}
	secret, err := s.access.Keys.Read(ctx, tx, s.access.Installation, *f.SecretID, secrets.MFATOTP)
	if err != nil {
		return mfaVerification{}, err
	}
	counter, valid := totpCounter(string(secret), input.Code, time.Now(), -1)
	clear(secret)
	return mfaVerification{valid: valid, counter: counter}, nil
}

func (s *mfaService) verifyWebAuthnEnrollment(ctx context.Context, tx pgx.Tx, f mfaFlow, input mfaProof) (mfaVerification, error) {
	if input.Method != "webauthn" || f.Data.WebAuthn == nil {
		return mfaVerification{}, nil
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(input.Credential)
	if err != nil {
		return mfaVerification{}, nil
	}
	user, err := s.mfaUser(ctx, tx, f.UserID)
	if err != nil {
		return mfaVerification{}, err
	}
	w, err := s.webAuthn()
	if err != nil {
		return mfaVerification{}, err
	}
	credential, err := w.CreateCredential(user, *f.Data.WebAuthn, parsed)
	if err != nil || len(credential.ID) == 0 || len(credential.ID) > 1024 {
		return mfaVerification{}, nil
	}
	return mfaVerification{valid: true, credential: credential}, nil
}

func (s *mfaService) completeMFAOutcome(r *http.Request, tx pgx.Tx, f mfaFlow, codes []string) (httpkit.Reply, error) {
	if f.Purpose == "login" || f.SessionID == nil {
		return s.completeMFALocalSession(r, tx, f.UserID, f.SessionID == nil, codes)
	}
	if f.Purpose == "manage" || f.Purpose == "reauth" {
		return s.completeMFARecentAuth(r, tx, f)
	}
	return s.completeMFAEnrollmentSession(r, tx, f, codes)
}

func (s *mfaService) completeMFALocalSession(r *http.Request, tx pgx.Tx, userID string, bootstrap bool, codes []string) (httpkit.Reply, error) {
	local, err := isLocalLoginEnabled(r.Context(), s.access, tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !local {
		return httpkit.Reply{}, problem.Fail(401, "local_login_disabled", "Local sign-in is unavailable.")
	}
	reply, err := s.sessions.newSession(r, tx, userID, sessionAuth{Method: "local", MFA: true})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if codes != nil && bootstrap {
		reply.Body = mfaBootstrapEnrolledResponse{Session: reply.Body, RecoveryCodes: codes}
	}
	return reply, nil
}

func (s *mfaService) completeMFARecentAuth(r *http.Request, tx pgx.Tx, f mfaFlow) (httpkit.Reply, error) {
	p, err := s.access.Authenticate(r, tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	purpose := f.Data.RecentPurpose
	if purpose == "" {
		purpose = "mfa_manage"
	}
	return s.sessions.grantRecent(r, tx, p, purpose, f.Data.RecentResource, true)
}

func (s *mfaService) completeMFAEnrollmentSession(r *http.Request, tx pgx.Tx, f mfaFlow, codes []string) (httpkit.Reply, error) {
	// Enrollment raises authentication strength: rotate the session rather
	// than upgrading a previously captured cookie in place.
	var method string
	if err := tx.QueryRow(r.Context(), "SELECT auth_method FROM olp.sessions WHERE id=$1", *f.SessionID).Scan(&method); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE id=$1", *f.SessionID); err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := s.sessions.newSession(r, tx, f.UserID, sessionAuth{Method: method, MFA: true})
	if err != nil {
		return httpkit.Reply{}, err
	}
	reply.Status = 200
	reply.Body = mfaEnrolledResponse{Enrolled: true, RecoveryCodes: codes}
	return reply, nil
}

func (s *mfaService) activateMFA(r *http.Request, tx pgx.Tx, f mfaFlow, credential *webauthn.Credential, counter int64) ([]string, error) {
	var count int
	if err := tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.mfa_factors WHERE user_id=$1", f.UserID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 10 {
		return nil, problem.Fail(409, "mfa_factor_limit", "At most ten factors may be enrolled.")
	}
	id, kind, credentialID, err := s.storeMFAFactorSecret(r.Context(), tx, f, credential)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.mfa_factors(id,user_id,kind,name,credential_id,last_counter) VALUES($1,$2,$3,$4,$5,$6)", id, f.UserID, kind, f.Data.Name, credentialID, counter); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", f.UserID); err != nil {
		return nil, err
	}
	if f.SessionID != nil {
		if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1 AND id<>$2", f.UserID, *f.SessionID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.sessions SET mfa_verified=true WHERE id=$1", *f.SessionID); err != nil {
			return nil, err
		}
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(f.UserID), "mfa.enroll", "mfa_factor", id, "success"); err != nil {
		return nil, err
	}
	if count == 0 {
		return s.replaceRecoveryCodes(r.Context(), tx, f.UserID)
	}
	return nil, nil
}

func (s *mfaService) storeMFAFactorSecret(ctx context.Context, tx pgx.Tx, f mfaFlow, credential *webauthn.Credential) (string, string, []byte, error) {
	if credential != nil {
		id := ids.New()
		raw, err := json.Marshal(credential)
		if err != nil {
			return "", "", nil, err
		}
		if err := s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.MFAWebAuthn, raw, nil); err != nil {
			return "", "", nil, err
		}
		return id, "webauthn", credential.ID, nil
	}
	if f.SecretID == nil {
		return "", "", nil, problem.Fail(401, "mfa_challenge_invalid", "Start enrollment again.")
	}
	if err := secrets.ClearExpiry(ctx, tx, *f.SecretID); err != nil {
		return "", "", nil, err
	}
	return *f.SecretID, "totp", nil, nil
}
