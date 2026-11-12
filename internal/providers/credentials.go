package providers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func (s *Server) credentials(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), `SELECT jsonb_build_object(
		'id',c.id,'version',c.version,
		'active',EXISTS(SELECT 1 FROM jsonb_array_elements(r.slots) slot WHERE slot->>'credential_id'=c.id::text),
		'draft_selected',EXISTS(SELECT 1 FROM olp.provider_slots d WHERE d.provider_id=p.id AND d.credential_id=c.id),
		'created_at',c.created_at,'revoked_at',c.revoked_at,
		'external_reference',c.external_reference,
		'grant',CASE WHEN c.plugin_digest IS NOT NULL THEN jsonb_build_object(
			'plugin_digest',c.plugin_digest,'principal',c.principal,'facts',c.grant_facts,'expires_at',g.expires_at,'lapsed_at',g.lapsed_at) END)
		FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id
		LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
		LEFT JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE c.provider_id=$1 AND c.id<$2 ORDER BY c.id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

type rotateRequest struct {
	Credential          string                 `json:"credential"`
	CredentialReference *secretstore.Reference `json:"credential_reference,omitempty"`
}

// rotate validates a new credential against the upstream, records it as the
// next version, and selects it for the draft. Published revisions keep the
// version they were activated with.
func (s *Server) rotate(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input rotateRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	var plain *string
	if input.Credential != "" {
		plain = &input.Credential
	}
	source, err := decodeCredential(plain, input.CredentialReference)
	if err != nil {
		return access.Reply{}, err
	}
	if source == nil {
		return access.Reply{}, access.Invalid("credential", "Supply a credential or external reference.")
	}
	tx, p, err := a.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = authorizeCredential(p, source); err != nil {
		return access.Reply{}, err
	}
	// The stored reply discloses the provider, so the caller must still reach
	// its project before a replay is served.
	current, err := readProvider(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	_, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if !current.Connection.Document().CredentialRequired() {
		return access.Reply{}, access.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
	}
	if current.Connection.Document().Grant() {
		return access.Reply{}, access.Fail(422, "credential_forbidden", grantEnrollmentOnly)
	}
	models, err := enabledModels(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	slots, err := loadSlots(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	defaultSlot := slots[slices.IndexFunc(slots, func(row slotRow) bool { return row.Default })]
	// Do not hold the installation mutation lock during the upstream probe.
	// The commit transaction rechecks authority, replay, and the draft ETag.
	if err = tx.Rollback(r.Context()); err != nil {
		return access.Reply{}, err
	}
	declared := make([]string, len(models))
	for i, model := range models {
		declared[i] = model.UpstreamModel
	}
	credential, err := s.material(r.Context(), source)
	if err != nil {
		return access.Reply{}, err
	}
	defer clear(credential)
	target := current.target(credential, declared...)
	if _, err = s.prober.ListModels(r.Context(), target); err != nil {
		return access.Reply{}, access.Fail(422, "credential_invalid", "The new credential was not accepted by the upstream: "+probe.Classify(err).Detail)
	}
	var validatedAt *time.Time
	if defaultSlot.validationFingerprint(current.Connection.Document(), models) != "" {
		if err = s.validateModelAccess(r.Context(), target, defaultSlot, models); err != nil {
			return access.Reply{}, access.Fail(422, "credential_invalid", "The new credential cannot access the enabled models: "+probe.Classify(err).Detail)
		}
		validatedAt = new(time.Now().UTC())
	}
	return access.Mutate(a, r, input, referencing(r.Context(), id, source), func(tx pgx.Tx, p access.Principal, locked *record) (access.Reply, error) {
		if locked.ETag != current.ETag {
			return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed during validation; reload and retry.")
		}
		rotated, err := s.rotateCredential(r.Context(), tx, current, models, defaultSlot, validatedAt, source)
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.credential.rotate", "provider_credential", rotated.ID, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: 201, ETag: rotated.ETag, Body: credentialChange{ProviderID: id, ETag: rotated.ETag, CredentialID: &rotated.ID, CredentialVersion: &rotated.Version}}, nil
	})
}

type credentialChange struct {
	ProviderID        string             `json:"provider_id"`
	ETag              string             `json:"etag"`
	CredentialID      *string            `json:"credential_id"`
	CredentialVersion *int               `json:"credential_version"`
	RuntimeGeneration *release.Published `json:"runtime_generation"`
}

// referencing locks a provider for a change that may store an external
// credential reference, which only settings administrators may name.
func referencing(ctx context.Context, id string, source credentialSource) func(pgx.Tx, access.Principal) (*record, error) {
	lock := changeable(ctx, id)
	return func(tx pgx.Tx, p access.Principal) (*record, error) {
		if err := authorizeCredential(p, source); err != nil {
			return nil, err
		}
		return lock(tx, p)
	}
}

// revoke marks a credential version unusable everywhere: drafts, published
// revisions, and gateways that learn it through the authority refresh. It
// ends the version's grant, if it has one.
func (s *Server) revoke(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.mutation(r, "provider.credential.revoke", nil, func(ctx context.Context, tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		credentialID, err := access.IDParam(r, "credential_id")
		if err != nil {
			return access.Reply{}, err
		}
		revoked, err := s.revokeCredential(ctx, tx, current, credentialID)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(credentialChange{ProviderID: current.ID, ETag: revoked.ETag, CredentialID: &credentialID, CredentialVersion: &revoked.Version, RuntimeGeneration: new(release.Published(revoked.Generation))}, revoked.ETag), nil
	})
}

type credentialRotation struct {
	ID      string
	Version int
	ETag    string
}

func (s *Server) rotateCredential(ctx context.Context, tx pgx.Tx, current *record, models []storedModel, defaultSlot slotRow, validatedAt *time.Time, source credentialSource) (credentialRotation, error) {
	id := current.ID
	credentialID, version, err := s.storeCredential(ctx, tx, id, source)
	if err != nil {
		return credentialRotation{}, err
	}
	fingerprint := defaultSlot.withCredential(credentialID).validationFingerprint(current.Connection.Document(), models)
	if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET credential_id=$2,validated_at=$4,validated_fingerprint=$3 WHERE provider_id=$1 AND is_default", id, credentialID, fingerprint, validatedAt); err != nil {
		return credentialRotation{}, err
	}
	etag, err := touchSlots(ctx, tx, id)
	if err != nil {
		return credentialRotation{}, err
	}
	return credentialRotation{ID: credentialID, Version: version, ETag: etag}, nil
}

type revokedCredential struct {
	Version    int
	Generation access.AuthorityGeneration
	ETag       string
}

func (s *Server) revokeCredential(ctx context.Context, tx pgx.Tx, current *record, credentialID string) (revokedCredential, error) {
	var version int
	err := tx.QueryRow(ctx, "UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL RETURNING version", credentialID, current.ID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.provider_credentials WHERE id=$1 AND provider_id=$2)", credentialID, current.ID).Scan(&exists); err != nil {
			return revokedCredential{}, err
		}
		if !exists {
			return revokedCredential{}, pgx.ErrNoRows
		}
		return revokedCredential{}, access.Fail(409, "already_revoked", "This credential version is already revoked.")
	}
	if err != nil {
		return revokedCredential{}, err
	}
	if err = grants.Revoke(ctx, tx, credentialID); err != nil {
		return revokedCredential{}, err
	}
	generation, err := access.AdvanceAuthority(ctx, tx)
	if err != nil {
		return revokedCredential{}, err
	}
	var referenced bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.provider_slots WHERE provider_id=$1 AND credential_id=$2)", current.ID, credentialID).Scan(&referenced); err != nil {
		return revokedCredential{}, err
	}
	etag := access.NewID()
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=draft_dirty OR $4,updated_at=now() WHERE id=$1", current.ID, etag, access.NewID(), referenced); err != nil {
		return revokedCredential{}, err
	}
	return revokedCredential{Version: version, Generation: generation, ETag: etag}, nil
}

const maxCredentialBytes = 65536

func ValidCredential(value string) error {
	if len(value) == 0 || len(value) > maxCredentialBytes || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") && !json.Valid([]byte(value)) {
		return access.Invalid("credential", "Use a credential of 1–65536 bytes; multiline credentials must be JSON.")
	}
	return nil
}

func (s *Server) StoreCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, int, error) {
	id, version, err := insertCredentialVersion(ctx, tx, providerID, nil)
	if err != nil {
		return "", 0, err
	}
	return id, version, s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.ProviderCredential, []byte(secret), nil)
}

type credentialSource interface{ credentialSource() }

type pastedCredential string

type externalCredential secretstore.Reference

func (pastedCredential) credentialSource() {}

func (externalCredential) credentialSource() {}

func decodeCredential(plain *string, reference *secretstore.Reference) (credentialSource, error) {
	switch {
	case plain != nil && reference != nil:
		return nil, access.Invalid("credential_reference", "Choose a sealed credential or an external reference.")
	case plain != nil:
		if err := ValidCredential(*plain); err != nil {
			return nil, err
		}
		return pastedCredential(*plain), nil
	case reference != nil:
		if reference.Validate() != nil {
			return nil, access.Invalid("credential_reference", "Use an immutable, version-pinned external credential reference.")
		}
		return externalCredential(*reference), nil
	}
	return nil, nil
}

// External references spend the installation's workload authority. An
// installation operator must approve the exact version for this provider;
// project managers may use approved versions but cannot introduce references.
func authorizeCredential(p access.Principal, source credentialSource) error {
	if _, external := source.(externalCredential); external {
		return p.Authorize(access.Settings)
	}
	return nil
}

func (s *Server) resolveReference(ctx context.Context, reference secretstore.Reference) ([]byte, error) {
	value, err := s.external.Resolve(ctx, reference)
	if err != nil {
		return nil, access.Fail(422, "external_secret_unavailable", "The external credential store could not resolve the pinned version.")
	}
	if err = ValidCredential(string(value)); err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}

func (s *Server) material(ctx context.Context, source credentialSource) ([]byte, error) {
	if reference, external := source.(externalCredential); external {
		return s.resolveReference(ctx, secretstore.Reference(reference))
	}
	return []byte(source.(pastedCredential)), nil
}

func (s *Server) storeCredential(ctx context.Context, tx pgx.Tx, providerID string, source credentialSource) (string, int, error) {
	if reference, external := source.(externalCredential); external {
		return s.StoreExternalCredential(ctx, tx, providerID, secretstore.Reference(reference))
	}
	return s.StoreCredential(ctx, tx, providerID, string(source.(pastedCredential)))
}

func (s *Server) StoreExternalCredential(ctx context.Context, tx pgx.Tx, providerID string, reference secretstore.Reference) (string, int, error) {
	data, err := json.Marshal(reference)
	if err != nil {
		return "", 0, err
	}
	return insertCredentialVersion(ctx, tx, providerID, data)
}

func insertCredentialVersion(ctx context.Context, tx pgx.Tx, providerID string, externalReference []byte) (string, int, error) {
	id := access.NewID()
	var version int
	err := tx.QueryRow(ctx, "INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3) RETURNING version", id, providerID, externalReference).Scan(&version)
	return id, version, err
}

func (s *Server) readCredential(ctx context.Context, q access.Queryer, id string) ([]byte, error) {
	var referenceJSON []byte
	if err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1", id).Scan(&referenceJSON); err != nil {
		return nil, err
	}
	if len(referenceJSON) > 0 {
		reference, err := secretstore.ParseReference(referenceJSON)
		if err != nil {
			return nil, release.ErrCredentialUnavailable
		}
		return s.resolveReference(ctx, reference)
	}
	return s.access.Keys.Read(ctx, q, s.access.Installation, id, secrets.ProviderCredential)
}

// BindDraftCredential stages an already stored destination credential. A new
// identity clears slot evidence and advances both mutation preconditions; a
// repeated binding is a no-op. The caller owns authorization and transaction.
func (s *Server) BindDraftCredential(ctx context.Context, tx pgx.Tx, providerID, slotID, credentialID string) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
  WHERE provider_id=$1 AND id=$2 AND credential_id IS DISTINCT FROM $3::uuid`, providerID, slotID, credentialID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = touchSlots(ctx, tx, providerID)
	return err
}

func (s *Server) CredentialEquals(ctx context.Context, q access.Queryer, credentialID, secret string) (bool, error) {
	current, err := s.access.Keys.Read(ctx, q, s.access.Installation, credentialID, secrets.ProviderCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(current)
	return sha256.Sum256(current) == sha256.Sum256([]byte(secret)), nil
}

func (s *Server) ExternalCredentialEquals(ctx context.Context, q access.Queryer, credentialID string, reference secretstore.Reference) (bool, error) {
	var data []byte
	err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1 AND revoked_at IS NULL", credentialID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var current secretstore.Reference
	return len(data) > 0 && json.Unmarshal(data, &current) == nil && current == reference, nil
}
