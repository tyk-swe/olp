package identity

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
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

func (s *Server) mfaStatus(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	rows, err := s.access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'kind',kind,'name',name,'created_at',created_at,'last_used_at',last_used_at) FROM olp.mfa_factors WHERE user_id=$1 ORDER BY created_at", p.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	factors, err := httpkit.JSONRows(rows)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var remaining int
	var revision string
	if err = s.access.Pool.QueryRow(r.Context(), "SELECT (SELECT count(*) FROM olp.mfa_recovery_codes WHERE user_id=$1),mfa_revision::text FROM olp.users WHERE id=$1", p.ID).Scan(&remaining, &revision); err != nil {
		return httpkit.Reply{}, err
	}
	required, err := ReadLocalMFARequired(r.Context(), s.access.Pool)
	_, webErr := s.webAuthn()
	return httpkit.Detail(map[string]any{"factors": factors, "recovery_codes_remaining": remaining, "required": required, "etag": revision, "webauthn_available": webErr == nil}, revision), err
}

func (s *Server) mfaManageChallenge(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
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

type mfaEnrollInput struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Challenge string `json:"challenge,omitempty"`
}

func (s *Server) mfaEnroll(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginMFAEnrollment(r, false)
}

func (s *Server) mfaBootstrap(r *http.Request) (httpkit.Reply, error) {
	return s.beginMFAEnrollment(r, true)
}
func (s *Server) beginMFAEnrollment(r *http.Request, bootstrap bool) (httpkit.Reply, error) {
	if bootstrap {
		if err := s.admit(r, "mfa_enrollment", ""); err != nil {
			return httpkit.Reply{}, err
		}
	}
	var input mfaEnrollInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Kind != "totp" && input.Kind != "webauthn" {
		return httpkit.Reply{}, httpkit.Invalid("kind", "Use totp or webauthn.")
	}
	if err := httpkit.ValidText("name", input.Name, 100); err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.access.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	f := mfaFlow{ID: httpkit.NewID(), Purpose: "enroll_" + input.Kind, Data: mfaFlowData{Name: input.Name}}
	if err := s.resolveMFAEnrollmentSubject(r, tx, bootstrap, input.Challenge, &f); err != nil {
		return httpkit.Reply{}, err
	}
	var count int
	var hasTOTP bool
	if err = tx.QueryRow(r.Context(), "SELECT count(*),COALESCE(bool_or(kind='totp'),false) FROM olp.mfa_factors WHERE user_id=$1", f.UserID).Scan(&count, &hasTOTP); err != nil {
		return httpkit.Reply{}, err
	}
	if count >= 10 || input.Kind == "totp" && hasTOTP {
		return httpkit.Reply{}, httpkit.Fail(409, "mfa_factor_limit", "Remove an existing authenticator before replacing it; at most ten factors and one TOTP authenticator are allowed.")
	}
	u, err := s.mfaUser(r.Context(), tx, f.UserID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	response, err := s.buildMFAEnrollmentChallenge(r, tx, input.Kind, u, &f)
	if err != nil {
		return httpkit.Reply{}, err
	}
	token, until, err := s.storeMFAFlow(r, tx, f)
	if err != nil {
		return httpkit.Reply{}, err
	}
	response["challenge"] = token
	response["expires_at"] = until
	return httpkit.OK(response), tx.Commit(r.Context())
}

func (s *Server) resolveMFAEnrollmentSubject(r *http.Request, tx pgx.Tx, bootstrap bool, challenge string, f *mfaFlow) error {
	if !bootstrap {
		p, err := s.access.Reauthorize(r, tx)
		if err != nil {
			return err
		}
		if err = s.access.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
			return err
		}
		f.UserID = p.ID
		f.SessionID = &p.SessionID
		return nil
	}
	parent, err := s.loadMFAFlow(r, tx, challenge)
	if err != nil {
		return err
	}
	enabled, err := mfaConfigured(r.Context(), tx, parent.UserID)
	if err != nil {
		return err
	}
	if parent.Purpose != "bootstrap" || enabled {
		return httpkit.Fail(401, "mfa_challenge_invalid", "Start sign-in again.")
	}
	f.UserID = parent.UserID
	_, err = tx.Exec(r.Context(), "DELETE FROM olp.mfa_challenges WHERE id=$1", parent.ID)
	return err
}

func (s *Server) buildMFAEnrollmentChallenge(r *http.Request, tx pgx.Tx, kind string, u mfaUser, f *mfaFlow) (map[string]any, error) {
	response := map[string]any{"kind": kind}
	if kind == "totp" {
		key, err := totp.Generate(totp.GenerateOpts{Issuer: "OpenLLMProxy", AccountName: u.Email, SecretSize: 32})
		if err != nil {
			return nil, err
		}
		id := httpkit.NewID()
		f.SecretID = &id
		expires := time.Now().Add(mfaTTL)
		if err = s.access.Keys.Store(r.Context(), tx, s.access.Installation, id, secrets.MFATOTP, []byte(key.Secret()), &expires); err != nil {
			return nil, err
		}
		picture, err := key.Image(256, 256)
		if err != nil {
			return nil, err
		}
		var image bytes.Buffer
		if err = png.Encode(&image, picture); err != nil {
			return nil, err
		}
		response["secret"] = key.Secret()
		response["otpauth_url"] = key.URL()
		response["qr_code"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(image.Bytes())
		return response, nil
	}
	w, err := s.webAuthn()
	if err != nil {
		return nil, httpkit.Invalid("kind", "Security keys require an HTTPS DNS public origin, or localhost for development.")
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(u.Credentials))
	for _, credential := range u.Credentials {
		exclude = append(exclude, credential.Descriptor())
	}
	options, session, err := w.BeginRegistration(u, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, err
	}
	session.Expires = time.Now().Add(mfaTTL)
	f.Data.WebAuthn = session
	response["public_key"] = options
	return response, nil
}

func (s *Server) mfaRemove(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "factor_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err = s.checkMFARevision(r, tx, p); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = s.access.ConsumeRecent(r, tx, p, "mfa_manage", ""); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var count int
		if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.mfa_factors WHERE user_id=$1", p.ID).Scan(&count); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		required, err := ReadLocalMFARequired(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if required && count <= 1 {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "mfa_required", "Enroll a replacement before removing your last required factor.")
		}
		deleted, err := tx.Exec(r.Context(), "DELETE FROM olp.mfa_factors WHERE id=$1 AND user_id=$2", id, p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if deleted.RowsAffected() != 1 {
			return httpkit.Reply{}, access.Event{}, pgx.ErrNoRows
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", p.ID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if count == 1 {
			if _, err = tx.Exec(r.Context(), "UPDATE olp.sessions SET mfa_verified=false WHERE user_id=$1 AND auth_method='local'", p.ID); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if _, err = tx.Exec(r.Context(), "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1", p.ID); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1 AND id<>$2", p.ID, p.SessionID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 204}, access.Event{Action: "mfa.remove", Resource: "mfa_factor", ID: id}, nil
	})
}

func (s *Server) checkMFARevision(r *http.Request, tx pgx.Tx, p authz.Principal) error {
	var revision string
	if err := tx.QueryRow(r.Context(), "SELECT mfa_revision::text FROM olp.users WHERE id=$1", p.ID).Scan(&revision); err != nil {
		return err
	}
	return httpkit.Match(r, revision)
}

func (s *Server) mfaRecovery(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
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
		if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1", p.ID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.OK(map[string]any{"recovery_codes": codes}), access.Event{Action: "mfa.recovery.rotate", Resource: "user", ID: p.ID}, nil
	})
}
