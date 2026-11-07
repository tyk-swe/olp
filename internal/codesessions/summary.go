package codesessions

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/secrets"
)

const summaryPrefix = "olp.summary.v1."

func (s *Store) SealCodeSummary(ctx context.Context, permit Permit, summary string) (string, error) {
	if summary == "" || len(summary) > codewire.MaxBody {
		return "", codemode.Refuse(502, "code_compaction_unavailable")
	}
	key, err := s.codeSummaryKey(ctx, permit, true)
	if err != nil {
		return "", err
	}
	aead, err := summaryCipher(key)
	if err != nil {
		return "", err
	}
	context, err := summaryContext(permit)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return summaryPrefix + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(summary), context)), nil
}

func (s *Store) OpenCodeSummary(ctx context.Context, permit Permit, envelope string) (string, error) {
	refused := codemode.Refuse(422, "code_compaction_unavailable")
	if !strings.HasPrefix(envelope, summaryPrefix) || len(envelope) > codewire.MaxBody {
		return "", refused
	}
	key, err := s.codeSummaryKey(ctx, permit, false)
	if err != nil {
		return "", refused
	}
	aead, err := summaryCipher(key)
	if err != nil {
		return "", refused
	}
	context, err := summaryContext(permit)
	if err != nil {
		return "", refused
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(envelope, summaryPrefix))
	if err != nil || len(ciphertext) < aead.NonceSize() {
		return "", refused
	}
	plaintext, err := aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], context)
	if err != nil {
		return "", refused
	}
	return string(plaintext), nil
}

func summaryCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func summaryContext(permit Permit) ([]byte, error) {
	return json.Marshal(struct {
		Version   int
		Root      string
		Account   string
		Principal string
	}{1, permit.Binding.RootID, permit.Account.ID, permit.Account.Principal})
}

func (s *Store) codeSummaryKey(ctx context.Context, permit Permit, create bool) ([]byte, error) {
	refused := codemode.Refuse(503, "code_compaction_unavailable")
	if s.keys == nil {
		return nil, refused
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, refused
	}
	defer tx.Rollback(ctx)
	if err := liveCodeTree(ctx, tx, permit.Binding); err != nil {
		return nil, err
	}
	var id string
	lookupErr := tx.QueryRow(ctx, `SELECT id::text FROM olp.code_summary_keys WHERE root_id=$1 AND account_id=$2 AND principal=$3`, permit.Binding.RootID, permit.Account.ID, permit.Account.Principal).Scan(&id)
	if lookupErr == nil {
		key, err := s.keys.Read(ctx, tx, s.installation, id, secrets.CodeSummaryKey)
		if err != nil {
			return nil, refused
		}
		return key, tx.Commit(ctx)
	}
	if lookupErr != pgx.ErrNoRows || !create {
		return nil, refused
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, refused
	}
	secretUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, refused
	}
	secretID := secretUUID.String()
	if err := s.keys.Store(ctx, tx, s.installation, secretID, secrets.CodeSummaryKey, key, nil); err != nil {
		return nil, refused
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_summary_keys(id,root_id,account_id,principal) VALUES($1,$2,$3,$4)`, secretID, permit.Binding.RootID, permit.Account.ID, permit.Account.Principal); err != nil {
		return nil, refused
	}
	return key, tx.Commit(ctx)
}
