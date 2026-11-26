package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/png"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type mfaFactorSummary struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type mfaStatusResponse struct {
	Factors                []mfaFactorSummary `json:"factors"`
	RecoveryCodesRemaining int                `json:"recovery_codes_remaining"`
	Required               bool               `json:"required"`
	ETag                   string             `json:"etag"`
	WebAuthnAvailable      bool               `json:"webauthn_available"`
}

type mfaEnrollInput struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Challenge string `json:"challenge,omitempty"`
}

type mfaEnrollmentResponse struct {
	Kind       string                       `json:"kind"`
	Challenge  string                       `json:"challenge"`
	ExpiresAt  time.Time                    `json:"expires_at"`
	Secret     string                       `json:"secret,omitempty"`
	OTPAuthURL string                       `json:"otpauth_url,omitempty"`
	QRCode     string                       `json:"qr_code,omitempty"`
	PublicKey  *protocol.CredentialCreation `json:"public_key,omitempty"`
}

type mfaRecoveryCodesResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

func (s *mfaService) register(mux *http.ServeMux) {
	s.access.Public(mux, "POST /api/v1/auth/mfa/verify", s.mfaVerify)
	s.access.Public(mux, "POST /api/v1/auth/mfa/enroll", s.mfaBootstrap)
	routes := map[string]access.Handler{
		"GET /api/v1/profile/mfa":                        s.mfaStatus,
		"POST /api/v1/profile/mfa/challenge":             s.mfaManageChallenge,
		"POST /api/v1/profile/mfa/enroll":                s.mfaEnroll,
		"POST /api/v1/profile/mfa/recovery-codes":        s.mfaRecovery,
		"DELETE /api/v1/profile/mfa/factors/{factor_id}": s.mfaRemove,
	}
	for pattern, fn := range routes {
		s.access.Route(mux, pattern, fn)
	}
}

func (s *mfaService) mfaStatus(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	factors, err := loadMFAFactors(r.Context(), s.access.Pool, p.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var remaining int
	var revision string
	if err := s.access.Pool.QueryRow(r.Context(), "SELECT (SELECT count(*) FROM olp.mfa_recovery_codes WHERE user_id=$1),mfa_revision::text FROM olp.users WHERE id=$1", p.ID).Scan(&remaining, &revision); err != nil {
		return httpkit.Reply{}, err
	}
	required, err := ReadLocalMFARequired(r.Context(), s.access.Pool)
	if err != nil {
		return httpkit.Reply{}, err
	}
	_, webErr := s.webAuthn()
	return httpkit.Detail(mfaStatusResponse{
		Factors:                factors,
		RecoveryCodesRemaining: remaining,
		Required:               required,
		ETag:                   revision,
		WebAuthnAvailable:      webErr == nil,
	}, revision), nil
}

func loadMFAFactors(ctx context.Context, q database.Queryer, userID string) ([]mfaFactorSummary, error) {
	rows, err := q.Query(ctx, "SELECT id::text,kind,name,created_at,last_used_at FROM olp.mfa_factors WHERE user_id=$1 ORDER BY created_at", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	factors := []mfaFactorSummary{}
	for rows.Next() {
		var factor mfaFactorSummary
		if err := rows.Scan(&factor.ID, &factor.Kind, &factor.Name, &factor.CreatedAt, &factor.LastUsedAt); err != nil {
			return nil, err
		}
		factors = append(factors, factor)
	}
	return factors, rows.Err()
}

func (s *mfaService) mfaManageChallenge(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		enabled, err := mfaConfigured(r.Context(), tx, p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !enabled {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(428, "reauthentication_required", "Confirm your password or linked identity before enrolling your first factor.")
		}
		reply, err := s.mfaChallenge(r, tx, p.ID, "manage", &p.SessionID, mfaFlowData{RecentPurpose: "mfa_manage"})
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return reply, access.Event{}, nil
	})
}

func validateMFAEnrollInput(input mfaEnrollInput) error {
	if input.Kind != "totp" && input.Kind != "webauthn" {
		return httpkit.Invalid("kind", "Use totp or webauthn.")
	}
	return httpkit.ValidText("name", input.Name, 100)
}

func decodeMFAEnrollInput(r *http.Request) (mfaEnrollInput, error) {
	var input mfaEnrollInput
	if err := httpkit.Decode(r, &input); err != nil {
		return mfaEnrollInput{}, err
	}
	if err := validateMFAEnrollInput(input); err != nil {
		return mfaEnrollInput{}, err
	}
	return input, nil
}

func (s *mfaService) mfaEnroll(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input, err := decodeMFAEnrollInput(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := s.access.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := s.startMFAEnrollment(r, tx, input, p.ID, &p.SessionID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return reply, access.Event{}, nil
	})
}

func (s *mfaService) mfaBootstrap(r *http.Request) (httpkit.Reply, error) {
	if err := admitAuth(s.access, r, "mfa_enrollment", ""); err != nil {
		return httpkit.Reply{}, err
	}
	input, err := decodeMFAEnrollInput(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		userID, err := s.consumeBootstrapChallenge(r, tx, input.Challenge)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		reply, err := s.startMFAEnrollment(r, tx, input, userID, nil)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		return reply, true, nil
	})
}

func (s *mfaService) consumeBootstrapChallenge(r *http.Request, tx pgx.Tx, challenge string) (string, error) {
	parent, err := s.loadMFAFlow(r, tx, challenge)
	if err != nil {
		return "", err
	}
	enabled, err := mfaConfigured(r.Context(), tx, parent.UserID)
	if err != nil {
		return "", err
	}
	if parent.Purpose != "bootstrap" || enabled {
		return "", httpkit.Fail(401, "mfa_challenge_invalid", "Start sign-in again.")
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.mfa_challenges WHERE id=$1", parent.ID); err != nil {
		return "", err
	}
	return parent.UserID, nil
}

func (s *mfaService) startMFAEnrollment(r *http.Request, tx pgx.Tx, input mfaEnrollInput, userID string, sessionID *string) (httpkit.Reply, error) {
	var count int
	var hasTOTP bool
	if err := tx.QueryRow(r.Context(), "SELECT count(*),COALESCE(bool_or(kind='totp'),false) FROM olp.mfa_factors WHERE user_id=$1", userID).Scan(&count, &hasTOTP); err != nil {
		return httpkit.Reply{}, err
	}
	if count >= 10 || input.Kind == "totp" && hasTOTP {
		return httpkit.Reply{}, httpkit.Fail(409, "mfa_factor_limit", "Remove an existing authenticator before replacing it; at most ten factors and one TOTP authenticator are allowed.")
	}
	u, err := s.mfaUser(r.Context(), tx, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	flow, response, err := s.buildMFAEnrollmentChallenge(r.Context(), tx, input, u, sessionID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	token, until, err := s.storeMFAFlow(r, tx, flow)
	if err != nil {
		return httpkit.Reply{}, err
	}
	response.Challenge = token
	response.ExpiresAt = until
	return httpkit.OK(response), nil
}

func (s *mfaService) buildMFAEnrollmentChallenge(ctx context.Context, tx pgx.Tx, input mfaEnrollInput, u mfaUser, sessionID *string) (mfaFlow, mfaEnrollmentResponse, error) {
	if input.Kind == "totp" {
		return s.buildTOTPEnrollment(ctx, tx, u, input.Name, sessionID)
	}
	return s.buildWebAuthnEnrollment(u, input.Name, sessionID)
}

func (s *mfaService) buildTOTPEnrollment(ctx context.Context, tx pgx.Tx, u mfaUser, name string, sessionID *string) (mfaFlow, mfaEnrollmentResponse, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "OpenLLMProxy", AccountName: u.Email, SecretSize: 32})
	if err != nil {
		return mfaFlow{}, mfaEnrollmentResponse{}, err
	}
	secretID := ids.New()
	expires := time.Now().Add(mfaTTL)
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, secretID, secrets.MFATOTP, []byte(key.Secret()), &expires); err != nil {
		return mfaFlow{}, mfaEnrollmentResponse{}, err
	}
	qrCode, err := encodeTOTPQRCode(key)
	if err != nil {
		return mfaFlow{}, mfaEnrollmentResponse{}, err
	}
	flow := mfaFlow{
		ID:        ids.New(),
		UserID:    u.ID,
		SessionID: sessionID,
		SecretID:  &secretID,
		Purpose:   "enroll_totp",
		Data:      mfaFlowData{Name: name},
	}
	response := mfaEnrollmentResponse{
		Kind:       "totp",
		Secret:     key.Secret(),
		OTPAuthURL: key.URL(),
		QRCode:     qrCode,
	}
	return flow, response, nil
}

func encodeTOTPQRCode(key *otp.Key) (string, error) {
	picture, err := key.Image(256, 256)
	if err != nil {
		return "", err
	}
	var image bytes.Buffer
	if err := png.Encode(&image, picture); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(image.Bytes()), nil
}

func (s *mfaService) buildWebAuthnEnrollment(u mfaUser, name string, sessionID *string) (mfaFlow, mfaEnrollmentResponse, error) {
	w, err := s.webAuthn()
	if err != nil {
		return mfaFlow{}, mfaEnrollmentResponse{}, httpkit.Invalid("kind", "Security keys require an HTTPS DNS public origin, or localhost for development.")
	}
	exclude := make([]protocol.CredentialDescriptor, len(u.Credentials))
	for i, credential := range u.Credentials {
		exclude[i] = credential.Descriptor()
	}
	options, session, err := w.BeginRegistration(u, webauthn.WithExclusions(exclude))
	if err != nil {
		return mfaFlow{}, mfaEnrollmentResponse{}, err
	}
	session.Expires = time.Now().Add(mfaTTL)
	flow := mfaFlow{
		ID:        ids.New(),
		UserID:    u.ID,
		SessionID: sessionID,
		Purpose:   "enroll_webauthn",
		Data:      mfaFlowData{Name: name, WebAuthn: session},
	}
	return flow, mfaEnrollmentResponse{Kind: "webauthn", PublicKey: options}, nil
}

func (s *mfaService) mfaRemove(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "factor_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := s.checkMFARevision(r, tx, p); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := s.access.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := s.deleteMFAFactor(r.Context(), tx, p, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 204}, access.Event{Action: "mfa.remove", Resource: "mfa_factor", ID: id}, nil
	})
}

func (s *mfaService) deleteMFAFactor(ctx context.Context, tx pgx.Tx, p authz.Principal, factorID string) error {
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.mfa_factors WHERE user_id=$1", p.ID).Scan(&count); err != nil {
		return err
	}
	required, err := ReadLocalMFARequired(ctx, tx)
	if err != nil {
		return err
	}
	if required && count <= 1 {
		return httpkit.Fail(409, "mfa_required", "Enroll a replacement before removing your last required factor.")
	}
	deleted, err := tx.Exec(ctx, "DELETE FROM olp.mfa_factors WHERE id=$1 AND user_id=$2", factorID, p.ID)
	if err != nil {
		return err
	}
	if deleted.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1", factorID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", p.ID); err != nil {
		return err
	}
	if count == 1 {
		if err := clearLastMFAFactorState(ctx, tx, p.ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.sessions WHERE user_id=$1 AND id<>$2", p.ID, p.SessionID)
	return err
}

func clearLastMFAFactorState(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, "UPDATE olp.sessions SET mfa_verified=false WHERE user_id=$1 AND auth_method='local'", userID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1", userID)
	return err
}

func (s *mfaService) checkMFARevision(r *http.Request, tx pgx.Tx, p authz.Principal) error {
	var revision string
	if err := tx.QueryRow(r.Context(), "SELECT mfa_revision::text FROM olp.users WHERE id=$1", p.ID).Scan(&revision); err != nil {
		return err
	}
	return httpkit.Match(r, revision)
}

func (s *mfaService) mfaRecovery(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := s.checkMFARevision(r, tx, p); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := s.access.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		configured, err := mfaConfigured(r.Context(), tx, p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !configured {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "mfa_not_enrolled", "Enroll a factor before creating recovery codes.")
		}
		codes, err := s.replaceRecoveryCodes(r.Context(), tx, p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", p.ID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.OK(mfaRecoveryCodesResponse{RecoveryCodes: codes}), access.Event{Action: "mfa.recovery.rotate", Resource: "user", ID: p.ID}, nil
	})
}
