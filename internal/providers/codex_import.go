package providers

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type codexImportInput struct {
	Credentials string `json:"credentials"`
	SlotID      string `json:"slot_id"`
}

type codexImportSlot struct {
	ID           string
	CredentialID *string
	Principal    string
}

func (s *Server) importCodexCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input codexImportInput
	if err := access.DecodeUnique(r, &input, 128<<10); err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	cfg := current.Configuration
	if !cfg.Grant() || cfg.ProfileID != codexauth.ProfileID {
		return access.Reply{}, access.Invalid("credentials", "This connection does not use the Codex subscription profile.")
	}
	credentials, err := codexauth.ParseCredentialImport(input.Credentials, time.Now())
	if err != nil {
		return access.Reply{}, access.Invalid("credentials", "Use the supported Codex auth.json format with matching account and user tokens.")
	}
	slot, err := s.codexImportSlot(r, id, input.SlotID)
	if err != nil {
		return access.Reply{}, err
	}
	client, err := s.connectionClient(r.Context(), &cfg, nil)
	if err != nil {
		return access.Reply{}, err
	}
	grant, err := s.Plugins.RefreshGrant(r.Context(), cfg.ProfileRevision, abi.Provider{Profile: cfg.ProfileID, Options: cfg.Options.PluginOptions}, credentials.RefreshRequest(), client, credentials.Secrets())
	if err != nil || !credentials.Matches(grant) || slot.CredentialID != nil && slot.Principal != grant.Principal {
		if auditErr := s.auditFailedGrantEnrollment(r, p.Actor(), id); auditErr != nil {
			return access.Reply{}, auditErr
		}
		return access.Reply{}, access.Invalid("credentials", "Could not verify the imported credentials for this account. Sign in again and retry.")
	}
	return s.stageCodexImport(r, current, slot, grant)
}

func (s *Server) codexImportSlot(r *http.Request, providerID, slotID string) (codexImportSlot, error) {
	if slotID != "" {
		if _, err := access.ParseUUID(slotID); err != nil {
			return codexImportSlot{}, access.Invalid("slot_id", "Use a credential slot identifier.")
		}
	}
	var slot codexImportSlot
	err := s.Access.Pool.QueryRow(r.Context(), `SELECT s.id::text,s.credential_id::text,coalesce(c.principal,'')
		FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id
		WHERE s.provider_id=$1 AND (($2='' AND s.is_default) OR s.id::text=$2)`, providerID, slotID).Scan(&slot.ID, &slot.CredentialID, &slot.Principal)
	return slot, err
}

func (s *Server) stageCodexImport(request *http.Request, current *record, slot codexImportSlot, grant abi.Grant) (access.Reply, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), stageGrantTimeout)
	defer cancel()
	r := request.WithContext(ctx)
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	locked, err := load(ctx, tx, current.ID, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(locked.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, locked.ETag); err != nil {
		return access.Reply{}, err
	}
	credentialID, version, err := grants.Store(ctx, tx, s.Access, current.ID, current.Configuration.ProfileRevision, codexauth.ProfileID, grant)
	if err != nil {
		return access.Reply{}, err
	}
	bound, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
		WHERE provider_id=$1 AND id=$2 AND credential_id IS NOT DISTINCT FROM $4::uuid`, current.ID, slot.ID, credentialID, slot.CredentialID)
	if err != nil {
		return access.Reply{}, err
	}
	if bound.RowsAffected() != 1 {
		return access.Reply{}, access.Fail(409, "codex_import_stale", "The credential slot changed during import. Reload before retrying.")
	}
	etag, err := touch(ctx, tx, current.ID)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", current.ID, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(ctx, tx, r, p.Actor(), "provider.grant.import", "provider_credential", credentialID, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 201, ETag: etag, Body: completion(current.ID, etag, grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal})})
}
