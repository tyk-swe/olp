package providers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/release"
)

type credentialGrantItem struct {
	PluginDigest string            `json:"plugin_digest"`
	Principal    string            `json:"principal"`
	Facts        map[string]string `json:"facts"`
	ExpiresAt    *time.Time        `json:"expires_at"`
	LapsedAt     *time.Time        `json:"lapsed_at"`
}

type credentialItem struct {
	ID                string                 `json:"id"`
	Version           int                    `json:"version"`
	Active            bool                   `json:"active"`
	DraftSelected     bool                   `json:"draft_selected"`
	CreatedAt         time.Time              `json:"created_at"`
	RevokedAt         *time.Time             `json:"revoked_at"`
	ExternalReference *secretstore.Reference `json:"external_reference"`
	Grant             *credentialGrantItem   `json:"grant"`
}

func scanCredentialItem(row pgx.CollectableRow) (credentialItem, error) {
	var (
		item                     credentialItem
		externalRef, grantFacts  []byte
		pluginDigest, principal  *string
		grantExpires, grantLapse *time.Time
	)
	if err := row.Scan(&item.ID, &item.Version, &item.Active, &item.DraftSelected, &item.CreatedAt, &item.RevokedAt, &externalRef, &pluginDigest, &principal, &grantFacts, &grantExpires, &grantLapse); err != nil {
		return credentialItem{}, err
	}
	if len(externalRef) > 0 {
		var ref secretstore.Reference
		if err := json.Unmarshal(externalRef, &ref); err != nil {
			return credentialItem{}, err
		}
		item.ExternalReference = &ref
	}
	if pluginDigest != nil {
		facts := map[string]string{}
		if len(grantFacts) > 0 {
			if err := json.Unmarshal(grantFacts, &facts); err != nil {
				return credentialItem{}, err
			}
			if facts == nil {
				facts = map[string]string{}
			}
		}
		item.Grant = &credentialGrantItem{
			PluginDigest: *pluginDigest,
			Principal:    ptr.Value(principal),
			Facts:        facts,
			ExpiresAt:    grantExpires,
			LapsedAt:     grantLapse,
		}
	}
	return item, nil
}

func (s *credentialService) credentials(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.access.Pool, p, id); err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), `SELECT c.id::text,c.version,
		EXISTS(SELECT 1 FROM jsonb_array_elements(r.slots) slot WHERE slot->>'credential_id'=c.id::text),
		EXISTS(SELECT 1 FROM olp.provider_slots d WHERE d.provider_id=p.id AND d.credential_id=c.id),
		c.created_at,c.revoked_at,c.external_reference,
		c.plugin_digest,c.principal,c.grant_facts,g.expires_at,g.lapsed_at
		FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id
		LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
		LEFT JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE c.provider_id=$1 AND c.id<$2 ORDER BY c.id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, scanCredentialItem)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item credentialItem) string { return item.ID }), nil
}

type rotateRequest struct {
	Credential          string                 `json:"credential"`
	CredentialReference *secretstore.Reference `json:"credential_reference,omitempty"`
}

// Published revisions keep the credential version they were activated with.
func (s *credentialService) rotate(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.access
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input rotateRequest
	if err = httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	var plain *string
	if input.Credential != "" {
		plain = &input.Credential
	}
	source, err := decodeCredential(plain, input.CredentialReference)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if source == nil {
		return httpkit.Reply{}, problem.Invalid("credential", "Supply a credential or external reference.")
	}
	// The stored reply discloses the provider, so the caller must still reach
	// its project before a replay is served. The upstream probe then runs
	// without the installation lock, and the commit rechecks the draft ETag.
	draft, replayed, err := access.Preflight(a, r, input, rotationDraftFor(r.Context(), id, source))
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	current, models := draft.provider, draft.models
	if err = httpkit.Match(r, current.ETag); err != nil {
		return httpkit.Reply{}, err
	}
	if !current.Connection.Document().CredentialRequired() {
		return httpkit.Reply{}, problem.Fail(422, "credential_forbidden", "This authentication mode takes no stored credential.")
	}
	if current.Connection.Document().Grant() {
		return httpkit.Reply{}, problem.Fail(422, "credential_forbidden", grantEnrollmentOnly)
	}
	slot, err := defaultSlot(draft.slots)
	if err != nil {
		return httpkit.Reply{}, err
	}
	declared := make([]string, len(models))
	for i, model := range models {
		declared[i] = model.UpstreamModel
	}
	credential, err := s.material(r.Context(), source)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer clear(credential)
	target := current.target(credential, declared...)
	if _, err = s.prober.ListModels(r.Context(), target); err != nil {
		return httpkit.Reply{}, problem.Fail(422, "credential_invalid", "The new credential was not accepted by the upstream: "+probe.Classify(err).Detail)
	}
	var validatedAt *time.Time
	fingerprint, err := slot.validationFingerprint(current.Connection.Document(), models)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if fingerprint != "" {
		if err = validateModelAccess(r.Context(), s.prober, target, *slot, models); err != nil {
			return httpkit.Reply{}, problem.Fail(422, "credential_invalid", "The new credential cannot access the enabled models: "+probe.Classify(err).Detail)
		}
		validatedAt = new(time.Now().UTC())
	}
	return access.Mutate(a, r, input, referencing(r.Context(), id, source), func(tx pgx.Tx, p authz.Principal, locked *record) (httpkit.Reply, access.Event, error) {
		if locked.ETag != current.ETag {
			return httpkit.Reply{}, access.Event{}, problem.Fail(412, "etag_mismatch", "The connection changed during validation; reload and retry.")
		}
		rotated, err := s.rotateCredential(r.Context(), tx, current, models, *slot, validatedAt, source)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 201, ETag: rotated.ETag, Body: credentialChange{ProviderID: id, ETag: rotated.ETag, CredentialID: &rotated.ID, CredentialVersion: &rotated.Version}}, access.Event{Action: "provider.credential.rotate", Resource: "provider_credential", ID: rotated.ID}, nil
	})
}

type rotationDraft struct {
	provider *record
	models   []storedModel
	slots    []slotRow
}

func rotationDraftFor(ctx context.Context, id string, source credentialSource) func(pgx.Tx, authz.Principal) (rotationDraft, error) {
	return func(tx pgx.Tx, p authz.Principal) (rotationDraft, error) {
		if err := authorizeCredential(p, source); err != nil {
			return rotationDraft{}, err
		}
		current, err := readProvider(ctx, tx, id)
		if err != nil {
			return rotationDraft{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return rotationDraft{}, err
		}
		models, err := enabledModels(ctx, tx, id)
		if err != nil {
			return rotationDraft{}, err
		}
		slots, err := loadSlots(ctx, tx, id)
		return rotationDraft{provider: current, models: models, slots: slots}, err
	}
}

type credentialChange struct {
	ProviderID        string             `json:"provider_id"`
	ETag              string             `json:"etag"`
	CredentialID      *string            `json:"credential_id"`
	CredentialVersion *int               `json:"credential_version"`
	RuntimeGeneration *release.Published `json:"runtime_generation"`
}

// Revoking makes a version unusable in drafts, published revisions and the
// gateways that learn it through the authority refresh, and ends its grant.
func (s *credentialService) revoke(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return providerMutation(s.access, r, "provider.credential.revoke", nil, func(ctx context.Context, tx pgx.Tx, p authz.Principal, current *record) (httpkit.Reply, error) {
		credentialID, err := httpkit.IDParam(r, "credential_id")
		if err != nil {
			return httpkit.Reply{}, err
		}
		revoked, err := s.revokeCredential(ctx, tx, current, credentialID)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Detail(credentialChange{ProviderID: current.ID, ETag: revoked.ETag, CredentialID: &credentialID, CredentialVersion: &revoked.Version, RuntimeGeneration: new(release.Published(revoked.Generation))}, revoked.ETag), nil
	})
}

type credentialRotation struct {
	ID      string
	Version int
	ETag    string
}

func (s *credentialService) rotateCredential(ctx context.Context, tx pgx.Tx, current *record, models []storedModel, slot slotRow, validatedAt *time.Time, source credentialSource) (credentialRotation, error) {
	id := current.ID
	credentialID, version, err := s.storeCredential(ctx, tx, id, source)
	if err != nil {
		return credentialRotation{}, err
	}
	fingerprint, err := slot.withCredential(credentialID).validationFingerprint(current.Connection.Document(), models)
	if err != nil {
		return credentialRotation{}, err
	}
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

func (s *credentialService) revokeCredential(ctx context.Context, tx pgx.Tx, current *record, credentialID string) (revokedCredential, error) {
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
		return revokedCredential{}, problem.Fail(409, "already_revoked", "This credential version is already revoked.")
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
	etag := ids.New()
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=draft_dirty OR $4,updated_at=now() WHERE id=$1", current.ID, etag, ids.New(), referenced); err != nil {
		return revokedCredential{}, err
	}
	return revokedCredential{Version: version, Generation: generation, ETag: etag}, nil
}

const maxCredentialBytes = 65536

func ValidCredential(value string) error {
	if len(value) == 0 || len(value) > maxCredentialBytes || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") && !json.Valid([]byte(value)) {
		return problem.Invalid("credential", "Use a credential of 1–65536 bytes; multiline credentials must be JSON.")
	}
	return nil
}

func (s *credentialService) storePlainCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, int, error) {
	id, version, err := insertCredentialVersion(ctx, tx, providerID, nil)
	if err != nil {
		return "", 0, err
	}
	return id, version, s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.ProviderCredential, []byte(secret), nil)
}

func (s *credentialService) resolveReference(ctx context.Context, reference secretstore.Reference) ([]byte, error) {
	value, err := s.external.Resolve(ctx, reference)
	if err != nil {
		return nil, problem.Fail(422, "external_secret_unavailable", "The external credential store could not resolve the pinned version.")
	}
	if err = ValidCredential(string(value)); err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}

func (s *credentialService) material(ctx context.Context, source credentialSource) ([]byte, error) {
	switch src := source.(type) {
	case externalCredential:
		return s.resolveReference(ctx, secretstore.Reference(src))
	case pastedCredential:
		return []byte(src), nil
	default:
		return nil, fmt.Errorf("unsupported credential source %T", source)
	}
}

func (s *credentialService) storeCredential(ctx context.Context, tx pgx.Tx, providerID string, source credentialSource) (string, int, error) {
	switch src := source.(type) {
	case externalCredential:
		return s.storeExternalCredential(ctx, tx, providerID, secretstore.Reference(src))
	case pastedCredential:
		return s.storePlainCredential(ctx, tx, providerID, string(src))
	default:
		return "", 0, fmt.Errorf("unsupported credential source %T", source)
	}
}

func (s *credentialService) storeExternalCredential(ctx context.Context, tx pgx.Tx, providerID string, reference secretstore.Reference) (string, int, error) {
	data, err := json.Marshal(reference)
	if err != nil {
		return "", 0, err
	}
	return insertCredentialVersion(ctx, tx, providerID, data)
}

func insertCredentialVersion(ctx context.Context, tx pgx.Tx, providerID string, externalReference []byte) (string, int, error) {
	id := ids.New()
	var version int
	err := tx.QueryRow(ctx, "INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3) RETURNING version", id, providerID, externalReference).Scan(&version)
	return id, version, err
}

func (s *credentialService) readLocalOrRef(ctx context.Context, q database.Queryer, id string) ([]byte, *secretstore.Reference, error) {
	var referenceJSON []byte
	if err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1", id).Scan(&referenceJSON); err != nil {
		return nil, nil, err
	}
	if len(referenceJSON) > 0 {
		reference, err := secretstore.ParseReference(referenceJSON)
		if err != nil {
			return nil, nil, release.ErrCredentialUnavailable
		}
		return nil, &reference, nil
	}
	secret, err := s.access.Keys.Read(ctx, q, s.access.Installation, id, secrets.ProviderCredential)
	return secret, nil, err
}

// The caller owns authorization and the transaction.
func (s *credentialService) bindDraftCredential(ctx context.Context, tx pgx.Tx, providerID, slotID, credentialID string) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
  WHERE provider_id=$1 AND id=$2 AND credential_id IS DISTINCT FROM $3::uuid`, providerID, slotID, credentialID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = touchSlots(ctx, tx, providerID)
	return err
}

func (s *credentialService) credentialEquals(ctx context.Context, q database.Queryer, credentialID, secret string) (bool, error) {
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

func (s *credentialService) externalCredentialEquals(ctx context.Context, q database.Queryer, credentialID string, reference secretstore.Reference) (bool, error) {
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
