package access

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

const mfaTTL = 5 * time.Minute

type mfaFlow struct {
	ID, UserID, UserETag, Revision, Purpose string
	SessionID, SecretID                     *string
	Data                                    mfaFlowData
}

type mfaFlowData struct {
	WebAuthn       *webauthn.SessionData `json:"webauthn,omitempty"`
	RecentPurpose  string                `json:"recent_purpose,omitempty"`
	RecentResource string                `json:"recent_resource,omitempty"`
	Name           string                `json:"name,omitempty"`
}

type mfaUser struct {
	User
	Credentials []webauthn.Credential
}

func (u mfaUser) WebAuthnID() []byte                         { return []byte(u.ID) }
func (u mfaUser) WebAuthnName() string                       { return u.Email }
func (u mfaUser) WebAuthnDisplayName() string                { return u.DisplayName }
func (u mfaUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }
func (s *Server) webAuthn() (*webauthn.WebAuthn, error) {
	origin, err := url.Parse(s.Origin)
	if err != nil {
		return nil, err
	}
	if origin.Scheme != "https" && !strings.EqualFold(origin.Hostname(), "localhost") && !strings.HasSuffix(strings.ToLower(origin.Hostname()), ".localhost") {
		return nil, errors.New("WebAuthn requires a secure DNS origin")
	}
	return webauthn.New(&webauthn.Config{RPDisplayName: "OpenLLMProxy", RPID: origin.Hostname(), RPOrigins: []string{s.Origin}, AttestationPreference: protocol.PreferNoAttestation, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
}

func (s *Server) mfaUser(ctx context.Context, tx pgx.Tx, id string) (mfaUser, error) {
	var user mfaUser
	u, err := scanUser(tx.QueryRow(ctx, "SELECT "+userColumns+" FROM olp.users u WHERE id=$1 AND active AND oidc_authorized", id))
	if err != nil {
		return user, err
	}
	user.User = u
	rows, err := tx.Query(ctx, "SELECT id::text FROM olp.mfa_factors WHERE user_id=$1 AND kind='webauthn' ORDER BY id FOR UPDATE", id)
	if err != nil {
		return user, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return user, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return user, err
	}
	for _, id := range ids {
		raw, err := s.Keys.Read(ctx, tx, s.Installation, id, secrets.MFAWebAuthn)
		if err != nil {
			return user, err
		}
		var credential webauthn.Credential
		if err = json.Unmarshal(raw, &credential); err != nil {
			return user, err
		}
		user.Credentials = append(user.Credentials, credential)
	}
	return user, nil
}

func mfaConfigured(ctx context.Context, q Queryer, user string) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.mfa_factors WHERE user_id=$1)", user).Scan(&enabled)
	return enabled, err
}

func mfaRequired(ctx context.Context, q Queryer) (bool, error) {
	var value string
	if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value FROM olp.settings WHERE key='auth.mfa_required'),'false')").Scan(&value); err != nil {
		return false, err
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("invalid local MFA policy")
	}
}

func (s *Server) storeMFAFlow(r *http.Request, tx pgx.Tx, f mfaFlow) (string, time.Time, error) {
	// Bound transient state and delete its expired enrollment secrets together.
	_, err := tx.Exec(r.Context(), `WITH expired AS(DELETE FROM olp.mfa_challenges WHERE expires_at<=clock_timestamp() OR (user_id=$1 AND id IN(SELECT id FROM olp.mfa_challenges WHERE user_id=$1 ORDER BY created_at DESC OFFSET 7)) RETURNING secret_id) DELETE FROM olp.secrets WHERE id IN(SELECT secret_id FROM expired) AND NOT EXISTS(SELECT 1 FROM olp.mfa_factors WHERE mfa_factors.id=secrets.id)`, f.UserID)
	if err != nil {
		return "", time.Time{}, err
	}
	token := secrets.Token()
	until := time.Now().Add(mfaTTL)
	data, _ := json.Marshal(f.Data)
	_, err = tx.Exec(r.Context(), `INSERT INTO olp.mfa_challenges(id,digest,user_id,user_etag,mfa_revision,session_id,purpose,data,secret_id,expires_at) SELECT $1,$2,u.id,u.etag,u.mfa_revision,$4,$5,$6,$7,$8 FROM olp.users u WHERE u.id=$3 AND active AND oidc_authorized`, f.ID, s.Auth.Digest(secrets.MFAChallengeDigest, token), f.UserID, f.SessionID, f.Purpose, data, f.SecretID, until)
	return token, until, err
}

func (s *Server) loadMFAFlow(r *http.Request, tx pgx.Tx, token string) (mfaFlow, error) {
	var f mfaFlow
	if len(token) > 256 {
		return f, Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
	err := tx.QueryRow(r.Context(), `SELECT c.id::text,c.user_id::text,c.user_etag::text,c.mfa_revision::text,c.session_id::text,c.secret_id::text,c.purpose,c.data FROM olp.mfa_challenges c JOIN olp.users u ON u.id=c.user_id WHERE c.digest=$1 AND c.expires_at>clock_timestamp() AND c.attempts<5 AND u.etag=c.user_etag AND u.mfa_revision=c.mfa_revision AND u.active AND u.oidc_authorized FOR UPDATE OF c`, s.Auth.Digest(secrets.MFAChallengeDigest, token)).Scan(&f.ID, &f.UserID, &f.UserETag, &f.Revision, &f.SessionID, &f.SecretID, &f.Purpose, &f.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
	if err != nil {
		return f, err
	}
	if f.SessionID == nil {
		enabled, e := s.localLoginEnabled(r, tx)
		if e != nil {
			return f, e
		}
		if !enabled {
			return f, Fail(401, "mfa_challenge_invalid", "Local sign-in is unavailable. Start authentication again.")
		}
	}
	if f.SessionID != nil {
		p, err := s.Authenticate(r, tx)
		if err != nil || p.SessionID != *f.SessionID || p.ID != f.UserID {
			return f, Fail(401, "mfa_challenge_invalid", "Start authentication again.")
		}
	}
	return f, nil
}

func (s *Server) mfaChallenge(r *http.Request, tx pgx.Tx, userID, purpose string, session *string, data mfaFlowData) (Reply, error) {
	u, err := s.mfaUser(r.Context(), tx, userID)
	if err != nil {
		return Reply{}, err
	}
	methods := []string{}
	var totp, recovery bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.mfa_factors WHERE user_id=$1 AND kind='totp'),EXISTS(SELECT 1 FROM olp.mfa_recovery_codes WHERE user_id=$1)", userID).Scan(&totp, &recovery); err != nil {
		return Reply{}, err
	}
	if totp {
		methods = append(methods, "totp")
	}
	if recovery {
		methods = append(methods, "recovery")
	}
	var options any
	w, webErr := s.webAuthn()
	if len(u.Credentials) > 0 && webErr == nil {
		creation, challenge, err := w.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return Reply{}, err
		}
		challenge.Expires = time.Now().Add(mfaTTL)
		data.WebAuthn = challenge
		options = creation
		methods = append(methods, "webauthn")
	}
	if purpose != "bootstrap" && len(methods) == 0 {
		return Reply{}, Fail(403, "mfa_recovery_required", "No enrolled factor is available at this public origin. Restore the WebAuthn origin or use offline account recovery.")
	}
	token, until, err := s.storeMFAFlow(r, tx, mfaFlow{ID: NewID(), UserID: userID, SessionID: session, Purpose: purpose, Data: data})
	if err != nil {
		return Reply{}, err
	}
	return Reply{Status: 202, Body: map[string]any{"challenge": token, "expires_at": until, "methods": methods, "enrollment_required": purpose == "bootstrap", "public_key": options, "webauthn_available": webErr == nil}}, nil
}

func (s *Server) localSession(r *http.Request, tx pgx.Tx, userID string) (Reply, error) {
	enabled, err := mfaConfigured(r.Context(), tx, userID)
	if err != nil {
		return Reply{}, err
	}
	required, err := mfaRequired(r.Context(), tx)
	if err != nil {
		return Reply{}, err
	}
	if enabled {
		return s.mfaChallenge(r, tx, userID, "login", nil, mfaFlowData{})
	}
	if required {
		return s.mfaChallenge(r, tx, userID, "bootstrap", nil, mfaFlowData{})
	}
	return s.newSession(r, tx, userID)
}

func (s *Server) replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1", userID); err != nil {
		return nil, err
	}
	codes := make([]string, 10)
	for i := range codes {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		value := strings.ToUpper(hex.EncodeToString(raw))
		codes[i] = value[:8] + "-" + value[8:16] + "-" + value[16:24] + "-" + value[24:]
		if _, err := tx.Exec(ctx, "INSERT INTO olp.mfa_recovery_codes(user_id,digest) VALUES($1,$2)", userID, s.Auth.Digest(secrets.MFARecoveryDigest, userID+":"+value)); err != nil {
			return nil, err
		}
	}
	return codes, nil
}
