package access

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"
	"github.com/tyk-swe/olp/internal/secrets"
)

func (s *Server) mfaStatus(r *http.Request, p Principal) (Reply, error) {
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'kind',kind,'name',name,'created_at',created_at,'last_used_at',last_used_at) FROM olp.mfa_factors WHERE user_id=$1 ORDER BY created_at", p.ID)
	if err != nil {
		return Reply{}, err
	}
	factors, err := JSONRows(rows)
	if err != nil {
		return Reply{}, err
	}
	var remaining int
	var revision string
	if err = s.Pool.QueryRow(r.Context(), "SELECT (SELECT count(*) FROM olp.mfa_recovery_codes WHERE user_id=$1),mfa_revision::text FROM olp.users WHERE id=$1", p.ID).Scan(&remaining, &revision); err != nil {
		return Reply{}, err
	}
	required, err := mfaRequired(r.Context(), s.Pool)
	_, webErr := s.webAuthn()
	return Detail(map[string]any{"factors": factors, "recovery_codes_remaining": remaining, "required": required, "etag": revision, "webauthn_available": webErr == nil}, revision), err
}

func (s *Server) mfaManageChallenge(r *http.Request, _ Principal) (Reply, error) {
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	enabled, err := mfaConfigured(r.Context(), tx, p.ID)
	if err != nil {
		return Reply{}, err
	}
	if !enabled {
		return Reply{}, Fail(428, "reauthentication_required", "Confirm your password or linked identity before enrolling your first factor.")
	}
	reply, err := s.mfaChallenge(r, tx, p.ID, "manage", &p.SessionID, mfaFlowData{RecentPurpose: "mfa_manage"})
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, reply)
}

type mfaEnrollInput struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Challenge string `json:"challenge,omitempty"`
}

func (s *Server) mfaEnroll(r *http.Request, _ Principal) (Reply, error) {
	return s.beginMFAEnrollment(r, false)
}

func (s *Server) mfaBootstrap(r *http.Request) (Reply, error) { return s.beginMFAEnrollment(r, true) }
func (s *Server) beginMFAEnrollment(r *http.Request, bootstrap bool) (Reply, error) {
	if bootstrap {
		if err := s.admit(r, "mfa_enrollment", ""); err != nil {
			return Reply{}, err
		}
	}
	var input mfaEnrollInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if input.Kind != "totp" && input.Kind != "webauthn" {
		return Reply{}, Invalid("kind", "Use totp or webauthn.")
	}
	if err := ValidText("name", input.Name, 100); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	f := mfaFlow{ID: NewID(), Purpose: "enroll_" + input.Kind, Data: mfaFlowData{Name: input.Name}}
	if bootstrap {
		parent, e := s.loadMFAFlow(r, tx, input.Challenge)
		if e != nil {
			return Reply{}, e
		}
		enabled, e := mfaConfigured(r.Context(), tx, parent.UserID)
		if e != nil {
			return Reply{}, e
		}
		if parent.Purpose != "bootstrap" || enabled {
			return Reply{}, Fail(401, "mfa_challenge_invalid", "Start sign-in again.")
		}
		f.UserID = parent.UserID
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.mfa_challenges WHERE id=$1", parent.ID); err != nil {
			return Reply{}, err
		}
	} else {
		p, e := s.Reauthorize(r, tx)
		if e != nil {
			return Reply{}, e
		}
		if err = s.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
			return Reply{}, err
		}
		f.UserID = p.ID
		f.SessionID = &p.SessionID
	}
	var count int
	var hasTOTP bool
	if err = tx.QueryRow(r.Context(), "SELECT count(*),COALESCE(bool_or(kind='totp'),false) FROM olp.mfa_factors WHERE user_id=$1", f.UserID).Scan(&count, &hasTOTP); err != nil {
		return Reply{}, err
	}
	if count >= 10 || input.Kind == "totp" && hasTOTP {
		return Reply{}, Fail(409, "mfa_factor_limit", "Remove an existing authenticator before replacing it; at most ten factors and one TOTP authenticator are allowed.")
	}
	u, err := s.mfaUser(r.Context(), tx, f.UserID)
	if err != nil {
		return Reply{}, err
	}
	response := map[string]any{"kind": input.Kind}
	if input.Kind == "totp" {
		key, e := totp.Generate(totp.GenerateOpts{Issuer: "OpenLLMProxy", AccountName: u.Email, SecretSize: 32})
		if e != nil {
			return Reply{}, e
		}
		id := NewID()
		f.SecretID = &id
		expires := time.Now().Add(mfaTTL)
		if err = s.Keys.Store(r.Context(), tx, s.Installation, id, secrets.MFATOTP, []byte(key.Secret()), &expires); err != nil {
			return Reply{}, err
		}
		picture, e := key.Image(256, 256)
		if e != nil {
			return Reply{}, e
		}
		var image bytes.Buffer
		if e = png.Encode(&image, picture); e != nil {
			return Reply{}, e
		}
		response["secret"] = key.Secret()
		response["otpauth_url"] = key.URL()
		response["qr_code"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(image.Bytes())
	} else {
		w, e := s.webAuthn()
		if e != nil {
			return Reply{}, Invalid("kind", "Security keys require an HTTPS DNS public origin, or localhost for development.")
		}
		exclude := make([]protocol.CredentialDescriptor, 0, len(u.Credentials))
		for _, credential := range u.Credentials {
			exclude = append(exclude, credential.Descriptor())
		}
		options, session, e := w.BeginRegistration(u, webauthn.WithExclusions(exclude))
		if e != nil {
			return Reply{}, e
		}
		session.Expires = time.Now().Add(mfaTTL)
		f.Data.WebAuthn = session
		response["public_key"] = options
	}
	token, until, err := s.storeMFAFlow(r, tx, f)
	if err != nil {
		return Reply{}, err
	}
	response["challenge"] = token
	response["expires_at"] = until
	return Commit(r, tx, OK(response))
}

func (s *Server) mfaRemove(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "factor_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = s.checkMFARevision(r, tx, p); err != nil {
		return Reply{}, err
	}
	if err = s.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
		return Reply{}, err
	}
	var count int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.mfa_factors WHERE user_id=$1", p.ID).Scan(&count); err != nil {
		return Reply{}, err
	}
	required, err := mfaRequired(r.Context(), tx)
	if err != nil {
		return Reply{}, err
	}
	if required && count <= 1 {
		return Reply{}, Fail(409, "mfa_required", "Enroll a replacement before removing your last required factor.")
	}
	deleted, err := tx.Exec(r.Context(), "DELETE FROM olp.mfa_factors WHERE id=$1 AND user_id=$2", id, p.ID)
	if err != nil {
		return Reply{}, err
	}
	if deleted.RowsAffected() != 1 {
		return Reply{}, pgx.ErrNoRows
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", id); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", p.ID); err != nil {
		return Reply{}, err
	}
	if count == 1 {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.sessions SET mfa_verified=false WHERE user_id=$1 AND auth_method='local'", p.ID); err != nil {
			return Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1", p.ID); err != nil {
			return Reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1 AND id<>$2", p.ID, p.SessionID); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "mfa.remove", "mfa_factor", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Reply{Status: 204})
}

func (s *Server) checkMFARevision(r *http.Request, tx pgx.Tx, p Principal) error {
	var revision string
	if err := tx.QueryRow(r.Context(), "SELECT mfa_revision::text FROM olp.users WHERE id=$1", p.ID).Scan(&revision); err != nil {
		return err
	}
	return Match(r, revision)
}

func (s *Server) mfaRecovery(r *http.Request, _ Principal) (Reply, error) {
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = s.checkMFARevision(r, tx, p); err != nil {
		return Reply{}, err
	}
	if err = s.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
		return Reply{}, err
	}
	configured, err := mfaConfigured(r.Context(), tx, p.ID)
	if err != nil {
		return Reply{}, err
	}
	if !configured {
		return Reply{}, Fail(409, "mfa_not_enrolled", "Enroll a factor before creating recovery codes.")
	}
	codes, err := s.replaceRecoveryCodes(r.Context(), tx, p.ID)
	if err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", p.ID); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "mfa.recovery.rotate", "user", p.ID, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, OK(map[string]any{"recovery_codes": codes}))
}
