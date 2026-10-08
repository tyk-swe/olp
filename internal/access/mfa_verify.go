package access

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/mfa"
	"github.com/tyk-swe/olp/internal/secrets"
)

type mfaProof struct {
	Challenge  string          `json:"challenge"`
	Method     string          `json:"method"`
	Code       string          `json:"code"`
	Credential json.RawMessage `json:"credential"`
}

func (s *Server) verifyMFAProof(r *http.Request, tx pgx.Tx, f mfaFlow, input mfaProof) (bool, error) {
	switch input.Method {
	case "totp":
		var id string
		var last int64
		err := tx.QueryRow(r.Context(), "SELECT id::text,last_counter FROM olp.mfa_factors WHERE user_id=$1 AND kind='totp' FOR UPDATE", f.UserID).Scan(&id, &last)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		secret, err := s.Keys.Read(r.Context(), tx, s.Installation, id, secrets.MFATOTP)
		if err != nil {
			return false, err
		}
		counter, valid := mfa.Counter(string(secret), input.Code, time.Now(), last)
		clear(secret)
		if !valid {
			return false, nil
		}
		_, err = tx.Exec(r.Context(), "UPDATE olp.mfa_factors SET last_counter=$2,last_used_at=now() WHERE id=$1", id, counter)
		return err == nil, err
	case "recovery":
		code := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(input.Code), "-", ""))
		if len(code) != 32 {
			return false, nil
		}
		deleted, err := tx.Exec(r.Context(), "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1 AND digest=$2", f.UserID, s.Auth.Digest(secrets.MFARecoveryDigest, f.UserID+":"+code))
		return err == nil && deleted.RowsAffected() == 1, err
	case "webauthn":
		if f.Data.WebAuthn == nil || len(input.Credential) == 0 {
			return false, nil
		}
		parsed, err := protocol.ParseCredentialRequestResponseBytes(input.Credential)
		if err != nil {
			return false, nil
		}
		user, err := s.mfaUser(r.Context(), tx, f.UserID)
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
		if err = tx.QueryRow(r.Context(), "SELECT id::text FROM olp.mfa_factors WHERE user_id=$1 AND credential_id=$2 FOR UPDATE", f.UserID, credential.ID).Scan(&id); err != nil {
			return false, err
		}
		raw, _ := json.Marshal(credential)
		if err = s.Keys.Store(r.Context(), tx, s.Installation, id, secrets.MFAWebAuthn, raw, nil); err != nil {
			return false, err
		}
		_, err = tx.Exec(r.Context(), "UPDATE olp.mfa_factors SET last_used_at=now() WHERE id=$1", id)
		return err == nil, err
	default:
		return false, nil
	}
}

func (s *Server) mfaFailure(r *http.Request, tx pgx.Tx, f mfaFlow) (Reply, error) {
	if _, err := tx.Exec(r.Context(), "UPDATE olp.mfa_challenges SET attempts=attempts+1 WHERE id=$1 AND attempts<5", f.ID); err != nil {
		return Reply{}, err
	}
	if err := Audit(r.Context(), tx, r, UserActor(f.UserID), "mfa.verify", "user", f.UserID, "failure"); err != nil {
		return Reply{}, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return Reply{}, err
	}
	return Reply{}, Fail(401, "mfa_verification_failed", "The verification did not succeed. Try another current factor or recovery code.")
}

func (s *Server) mfaVerify(r *http.Request) (Reply, error) {
	var input mfaProof
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if err := s.admit(r, "mfa_request", ""); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	f, err := s.loadMFAFlow(r, tx, input.Challenge)
	if err != nil {
		return Reply{}, err
	}
	if err = s.admit(r, "mfa_verify", f.UserID); err != nil {
		return Reply{}, err
	}
	var valid bool
	var credential *webauthn.Credential
	var counter int64
	switch f.Purpose {
	case "login", "manage", "reauth":
		valid, err = s.verifyMFAProof(r, tx, f, input)
	case "enroll_totp":
		if input.Method == "totp" && f.SecretID != nil {
			var secret []byte
			secret, err = s.Keys.Read(r.Context(), tx, s.Installation, *f.SecretID, secrets.MFATOTP)
			if err == nil {
				counter, valid = mfa.Counter(string(secret), input.Code, time.Now(), -1)
				clear(secret)
			}
		}
	case "enroll_webauthn":
		if input.Method == "webauthn" && f.Data.WebAuthn != nil {
			var parsed *protocol.ParsedCredentialCreationData
			parsed, err = protocol.ParseCredentialCreationResponseBytes(input.Credential)
			if err != nil {
				err = nil
				break
			}
			var user mfaUser
			user, err = s.mfaUser(r.Context(), tx, f.UserID)
			if err != nil {
				break
			}
			var w *webauthn.WebAuthn
			w, err = s.webAuthn()
			if err != nil {
				break
			}
			credential, err = w.CreateCredential(user, *f.Data.WebAuthn, parsed)
			valid = err == nil && len(credential.ID) > 0 && len(credential.ID) <= 1024
			err = nil
		}
	default:
		return Reply{}, Fail(401, "mfa_challenge_invalid", "Start authentication again.")
	}
	if err != nil {
		return Reply{}, err
	}
	if !valid {
		return s.mfaFailure(r, tx, f)
	}
	var codes []string
	if strings.HasPrefix(f.Purpose, "enroll_") {
		codes, err = s.activateMFA(r, tx, f, credential, counter)
		if err != nil {
			return Reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.mfa_challenges WHERE id=$1", f.ID); err != nil {
		return Reply{}, err
	}
	var reply Reply
	switch {
	case f.Purpose == "login" || f.SessionID == nil:
		if local, e := s.localLoginEnabled(r, tx); e != nil {
			return Reply{}, e
		} else if !local {
			return Reply{}, Fail(401, "local_login_disabled", "Local sign-in is unavailable.")
		}
		reply, err = s.newSession(r, tx, f.UserID, sessionAuth{Method: "local", MFA: true})
	case f.Purpose == "manage" || f.Purpose == "reauth":
		p, e := s.Authenticate(r, tx)
		if e != nil {
			return Reply{}, e
		}
		purpose := f.Data.RecentPurpose
		if purpose == "" {
			purpose = "mfa_manage"
		}
		reply, err = s.grantRecent(r, tx, p, purpose, f.Data.RecentResource, true)
	default:
		// Enrollment raises authentication strength: rotate the session rather
		// than upgrading a previously captured cookie in place.
		var method string
		if err = tx.QueryRow(r.Context(), "SELECT auth_method FROM olp.sessions WHERE id=$1", *f.SessionID).Scan(&method); err != nil {
			return Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE id=$1", *f.SessionID); err != nil {
			return Reply{}, err
		}
		reply, err = s.newSession(r, tx, f.UserID, sessionAuth{Method: method, MFA: true})
		reply.Status = 200
		reply.Body = map[string]any{"enrolled": true, "recovery_codes": codes}
	}
	if err != nil {
		return Reply{}, err
	}
	if codes != nil && f.SessionID == nil {
		reply.Body = map[string]any{"session": reply.Body, "recovery_codes": codes}
	}
	if err = Audit(r.Context(), tx, r, UserActor(f.UserID), "mfa.verify", "user", f.UserID, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, reply)
}

func (s *Server) activateMFA(r *http.Request, tx pgx.Tx, f mfaFlow, credential *webauthn.Credential, counter int64) ([]string, error) {
	var count int
	if err := tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.mfa_factors WHERE user_id=$1", f.UserID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 10 {
		return nil, Fail(409, "mfa_factor_limit", "At most ten factors may be enrolled.")
	}
	id := NewID()
	kind := "webauthn"
	var credentialID []byte
	if credential != nil {
		credentialID = credential.ID
		raw, _ := json.Marshal(credential)
		if err := s.Keys.Store(r.Context(), tx, s.Installation, id, secrets.MFAWebAuthn, raw, nil); err != nil {
			return nil, err
		}
	} else {
		if f.SecretID == nil {
			return nil, Fail(401, "mfa_challenge_invalid", "Start enrollment again.")
		}
		id = *f.SecretID
		kind = "totp"
		if _, err := tx.Exec(r.Context(), "UPDATE olp.secrets SET expires_at=NULL WHERE id=$1", id); err != nil {
			return nil, err
		}
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
	if err := Audit(r.Context(), tx, r, UserActor(f.UserID), "mfa.enroll", "mfa_factor", id, "success"); err != nil {
		return nil, err
	}
	if count == 0 {
		return s.replaceRecoveryCodes(r.Context(), tx, f.UserID)
	}
	return nil, nil
}
