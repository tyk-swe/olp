package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const mfaTTL = 5 * time.Minute

const mfaFlowSelectSQL = `SELECT c.id::text,c.user_id::text,c.user_etag::text,c.mfa_revision::text,c.session_id::text,c.secret_id::text,c.purpose,c.data FROM olp.mfa_challenges c JOIN olp.users u ON u.id=c.user_id WHERE c.digest=$1 AND c.expires_at>clock_timestamp() AND c.attempts<5 AND u.etag=c.user_etag AND u.mfa_revision=c.mfa_revision AND u.active AND u.oidc_authorized`

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

type mfaChallengeResponse struct {
	Challenge          string                        `json:"challenge"`
	ExpiresAt          time.Time                     `json:"expires_at"`
	Methods            []string                      `json:"methods"`
	EnrollmentRequired bool                          `json:"enrollment_required"`
	PublicKey          *protocol.CredentialAssertion `json:"public_key"`
	WebAuthnAvailable  bool                          `json:"webauthn_available"`
}

type mfaUser struct {
	authz.User
	Credentials []webauthn.Credential
}

func (u mfaUser) WebAuthnID() []byte                         { return []byte(u.ID) }
func (u mfaUser) WebAuthnName() string                       { return u.Email }
func (u mfaUser) WebAuthnDisplayName() string                { return u.DisplayName }
func (u mfaUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

func (s *mfaService) webAuthn() (*webauthn.WebAuthn, error) {
	return webAuthnForOrigin(s.access.Origin)
}

func webAuthnForOrigin(rawOrigin string) (*webauthn.WebAuthn, error) {
	origin, err := url.Parse(rawOrigin)
	if err != nil {
		return nil, err
	}
	if !isAllowedWebAuthnOrigin(origin) {
		return nil, errors.New("WebAuthn requires a secure DNS origin")
	}
	return webauthn.New(&webauthn.Config{
		RPDisplayName:          "OpenLLMProxy",
		RPID:                   strings.ToLower(origin.Hostname()),
		RPOrigins:              []string{rawOrigin},
		AttestationPreference:  protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired},
	})
}

func isAllowedWebAuthnOrigin(origin *url.URL) bool {
	if origin == nil || origin.Hostname() == "" {
		return false
	}
	if origin.Scheme == "https" {
		return true
	}
	if origin.Scheme != "http" {
		return false
	}
	host := strings.ToLower(origin.Hostname())
	return host == "localhost" || (len(host) > len(".localhost") && strings.HasSuffix(host, ".localhost"))
}

func (s *mfaService) mfaUser(ctx context.Context, tx pgx.Tx, userID string) (mfaUser, error) {
	u, err := access.ScanUser(tx.QueryRow(ctx, "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1 AND active AND oidc_authorized", userID))
	if err != nil {
		return mfaUser{}, err
	}
	factorIDs, err := loadWebAuthnFactorIDs(ctx, tx, userID)
	if err != nil {
		return mfaUser{}, err
	}
	credentials := make([]webauthn.Credential, 0, len(factorIDs))
	for _, factorID := range factorIDs {
		raw, err := s.access.Keys.Read(ctx, tx, s.access.Installation, factorID, secrets.MFAWebAuthn)
		if err != nil {
			return mfaUser{}, err
		}
		var credential webauthn.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return mfaUser{}, err
		}
		credentials = append(credentials, credential)
	}
	return mfaUser{User: u, Credentials: credentials}, nil
}

func loadWebAuthnFactorIDs(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT id::text FROM olp.mfa_factors WHERE user_id=$1 AND kind='webauthn' ORDER BY id FOR UPDATE", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var factorIDs []string
	for rows.Next() {
		var factorID string
		if err := rows.Scan(&factorID); err != nil {
			return nil, err
		}
		factorIDs = append(factorIDs, factorID)
	}
	return factorIDs, rows.Err()
}

func ReadLocalMFARequired(ctx context.Context, q database.Queryer) (bool, error) {
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

func SetLocalMFARequired(ctx context.Context, tx pgx.Tx, p authz.Principal, required bool) error {
	var current string
	if err := tx.QueryRow(ctx, "SELECT value FROM olp.settings WHERE key='auth.mfa_required' FOR UPDATE").Scan(&current); err != nil {
		return err
	}
	desired := strconv.FormatBool(required)
	if current == desired {
		return nil
	}
	if err := p.Authorize(authz.Access); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE olp.settings SET value=$1,etag=$2,updated_by=$3,updated_at=now() WHERE key='auth.mfa_required'", desired, ids.New(), p.UserID())
	return err
}

func (s *mfaService) storeMFAFlow(r *http.Request, tx pgx.Tx, f mfaFlow) (string, time.Time, error) {
	// Bound transient state and delete its expired enrollment secrets together.
	_, err := tx.Exec(r.Context(), `WITH expired AS(DELETE FROM olp.mfa_challenges WHERE expires_at<=clock_timestamp() OR (user_id=$1 AND id IN(SELECT id FROM olp.mfa_challenges WHERE user_id=$1 ORDER BY created_at DESC OFFSET 7)) RETURNING secret_id) DELETE FROM olp.secrets WHERE id IN(SELECT secret_id FROM expired) AND NOT EXISTS(SELECT 1 FROM olp.mfa_factors WHERE mfa_factors.id=secrets.id)`, f.UserID)
	if err != nil {
		return "", time.Time{}, err
	}
	token := secrets.Token()
	until := time.Now().Add(mfaTTL)
	data, err := json.Marshal(f.Data)
	if err != nil {
		return "", time.Time{}, err
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO olp.mfa_challenges(id,digest,user_id,user_etag,mfa_revision,session_id,purpose,data,secret_id,expires_at) SELECT $1,$2,u.id,u.etag,u.mfa_revision,$4,$5,$6,$7,$8 FROM olp.users u WHERE u.id=$3 AND active AND oidc_authorized`, f.ID, s.access.Auth.Digest(secrets.MFAChallengeDigest, token), f.UserID, f.SessionID, f.Purpose, data, f.SecretID, until)
	return token, until, err
}

func (s *mfaService) loadMFAFlow(r *http.Request, tx pgx.Tx, token string) (mfaFlow, error) {
	return s.readMFAFlow(r, tx, token, true)
}

func (s *mfaService) readMFAFlow(r *http.Request, q database.Queryer, token string, lock bool) (mfaFlow, error) {
	var f mfaFlow
	if len(token) > 256 {
		return f, problem.Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
	query := mfaFlowSelectSQL
	if lock {
		query += " FOR UPDATE OF c"
	}
	err := q.QueryRow(r.Context(), query, s.access.Auth.Digest(secrets.MFAChallengeDigest, token)).Scan(&f.ID, &f.UserID, &f.UserETag, &f.Revision, &f.SessionID, &f.SecretID, &f.Purpose, &f.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, problem.Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
	if err != nil {
		return f, err
	}
	if f.SessionID == nil {
		enabled, e := isLocalLoginEnabled(r.Context(), s.access, q)
		if e != nil {
			return f, e
		}
		if !enabled {
			return f, problem.Fail(401, "mfa_challenge_invalid", "Local sign-in is unavailable. Start authentication again.")
		}
		return f, nil
	}
	p, err := s.access.Authenticate(r, q)
	if err != nil || p.SessionID != *f.SessionID || p.ID != f.UserID {
		return f, problem.Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
	return f, nil
}

func (s *mfaService) mfaChallenge(r *http.Request, tx pgx.Tx, userID, purpose string, session *string, data mfaFlowData) (httpkit.Reply, error) {
	u, err := s.mfaUser(r.Context(), tx, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	methods, err := availableMFAMethods(r.Context(), tx, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	flowData := data
	var options *protocol.CredentialAssertion
	w, webErr := s.webAuthn()
	if len(u.Credentials) > 0 && webErr == nil {
		assertion, challenge, err := w.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return httpkit.Reply{}, err
		}
		challenge.Expires = time.Now().Add(mfaTTL)
		flowData.WebAuthn = challenge
		options = assertion
		methods = append(methods, "webauthn")
	}
	if purpose != "bootstrap" && len(methods) == 0 {
		return httpkit.Reply{}, problem.Fail(403, "mfa_recovery_required", "No enrolled factor is available at this public origin. Restore the WebAuthn origin or use offline account recovery.")
	}
	token, until, err := s.storeMFAFlow(r, tx, mfaFlow{ID: ids.New(), UserID: userID, SessionID: session, Purpose: purpose, Data: flowData})
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Reply{
		Status: 202,
		Body: mfaChallengeResponse{
			Challenge:          token,
			ExpiresAt:          until,
			Methods:            methods,
			EnrollmentRequired: purpose == "bootstrap",
			PublicKey:          options,
			WebAuthnAvailable:  webErr == nil,
		},
	}, nil
}

func availableMFAMethods(ctx context.Context, q database.Queryer, userID string) ([]string, error) {
	var hasTOTP, hasRecovery bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.mfa_factors WHERE user_id=$1 AND kind='totp'),EXISTS(SELECT 1 FROM olp.mfa_recovery_codes WHERE user_id=$1)", userID).Scan(&hasTOTP, &hasRecovery); err != nil {
		return nil, err
	}
	methods := []string{}
	if hasTOTP {
		methods = append(methods, "totp")
	}
	if hasRecovery {
		methods = append(methods, "recovery")
	}
	return methods, nil
}

func (s *mfaService) localSession(r *http.Request, tx pgx.Tx, userID string) (httpkit.Reply, error) {
	enabled, err := mfaConfigured(r.Context(), tx, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	required, err := ReadLocalMFARequired(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if enabled {
		return s.mfaChallenge(r, tx, userID, "login", nil, mfaFlowData{})
	}
	if required {
		return s.mfaChallenge(r, tx, userID, "bootstrap", nil, mfaFlowData{})
	}
	return s.sessions.newSession(r, tx, userID)
}

func formatRecoveryCode(raw [16]byte) (string, string) {
	normalized := strings.ToUpper(hex.EncodeToString(raw[:]))
	formatted := normalized[:8] + "-" + normalized[8:16] + "-" + normalized[16:24] + "-" + normalized[24:]
	return formatted, normalized
}

func normalizeRecoveryCode(input string) (string, bool) {
	code := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(input), "-", ""))
	if len(code) != 32 {
		return "", false
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return code, true
}

func (s *mfaService) replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1", userID); err != nil {
		return nil, err
	}
	codes := make([]string, 10)
	for i := range codes {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		formatted, normalized := formatRecoveryCode(raw)
		codes[i] = formatted
		if _, err := tx.Exec(ctx, "INSERT INTO olp.mfa_recovery_codes(user_id,digest) VALUES($1,$2)", userID, s.access.Auth.Digest(secrets.MFARecoveryDigest, userID+":"+normalized)); err != nil {
			return nil, err
		}
	}
	return codes, nil
}
