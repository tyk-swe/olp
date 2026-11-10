package providers

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/auth"
	codevendor "github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/plugin/abi"
)

type credentialImportInput struct {
	Credentials string `json:"credentials"`
	SlotID      string `json:"slot_id"`
}

type credentialImportSlot struct {
	ID           string
	CredentialID *string
	Principal    string
}

func (s *Server) importCodexCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.importCredentials(r, p, code.AdapterCodex)
}

func (s *Server) importClaudeCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.importCredentials(r, p, code.AdapterClaude)
}

func (s *Server) importAntigravityCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.importCredentials(r, p, code.AdapterAntigravity)
}

func (s *Server) importCredentials(r *http.Request, p access.Principal, adapter code.Adapter) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input credentialImportInput
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
	selectedVendor, supported := codevendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if !supported || selectedVendor.Adapter != adapter {
		return access.Reply{}, access.Invalid("credentials", "This connection does not use the selected subscription profile.")
	}
	if err := auth.ValidateProviderConfig(r.Context(), s.Plugins, cfg.Identity, providerconfig.Options{
		SemanticHeaders:   cfg.Options.SemanticHeaders,
		QuerySettings:     cfg.Options.QuerySettings,
		OperationDefaults: cfg.Options.OperationDefaults,
		Bindings:          cfg.Options.Bindings,
		Models:            cfg.Options.Models,
		ParameterDefaults: cfg.Options.ParameterDefaults,
		CredentialHeaders: cfg.Options.CredentialHeaders,
		PluginOptions:     cfg.Options.PluginOptions,
	}, selectedVendor); err != nil {
		return access.Reply{}, access.Invalid("credentials", "This connection must use the unmodified subscription plugin and configuration.")
	}
	slot, err := s.credentialImportSlot(r, id, input.SlotID)
	if err != nil {
		return access.Reply{}, err
	}
	client, err := s.connectionClient(r.Context(), &cfg, nil)
	if err != nil {
		return access.Reply{}, err
	}
	credentials, err := codevendor.PrepareImport(r.Context(), adapter, input.Credentials, cfg.Options.PluginOptions["client_id"], client, time.Now())
	if err != nil {
		return access.Reply{}, access.Invalid("credentials", "Could not verify the supported credential format and original account. Sign in again and retry.")
	}
	grant, err := s.Plugins.RefreshGrant(r.Context(), cfg.ProfileRevision, abi.Provider{Profile: cfg.ProfileID, Options: cfg.Options.PluginOptions}, credentials.Refresh, client, credentials.Secrets)
	if err != nil || grants.ValidateTokenMaterial(grant.AccessToken, grant.RefreshToken, grant.ExpiresIn) != nil || !credentials.Matches(grant) || slot.CredentialID != nil && slot.Principal != grant.Principal {
		if auditErr := s.auditFailedGrantEnrollment(r, p.Actor(), id); auditErr != nil {
			return access.Reply{}, auditErr
		}
		return access.Reply{}, access.Invalid("credentials", "Could not verify the imported credentials for this account. Sign in again and retry.")
	}
	verified := abi.Grant{
		AccessToken: grant.AccessToken, RefreshToken: cmp.Or(grant.RefreshToken, credentials.Refresh.RefreshToken),
		ExpiresIn: grant.ExpiresIn, Principal: grant.Principal, Facts: grant.Facts,
	}
	return s.stageCredentialImport(r, current, slot, verified, adapter)
}

func (s *Server) credentialImportSlot(r *http.Request, providerID, slotID string) (credentialImportSlot, error) {
	identifier, parseErr := uuid.Parse(slotID)
	if slotID != "" && parseErr != nil {
		return credentialImportSlot{}, access.Invalid("slot_id", "Use a credential slot identifier.")
	}
	var slot credentialImportSlot
	err := s.Access.Pool.QueryRow(r.Context(), `SELECT s.id::text,s.credential_id::text,coalesce(c.principal,'')
		FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id
		WHERE s.provider_id=$1 AND (($3 AND s.is_default) OR (NOT $3 AND s.id=$2))`, providerID, identifier, slotID == "").Scan(&slot.ID, &slot.CredentialID, &slot.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialImportSlot{}, access.Invalid("slot_id", "Unknown credential slot for this connection.")
	}
	return slot, err
}

func (s *Server) stageCredentialImport(request *http.Request, current *record, slot credentialImportSlot, grant abi.Grant, adapter code.Adapter) (access.Reply, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), stageGrantTimeout)
	defer cancel()
	r := request.WithContext(ctx)
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
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
	credentialID, version, err := grants.Store(ctx, tx, s.Access, current.ID, current.Configuration.ProfileRevision, current.Configuration.ProfileID, grant)
	if err != nil {
		return access.Reply{}, err
	}
	return s.bindGrant(r, tx, p.Actor(), grantBinding{
		ProviderID: current.ID, SlotID: slot.ID, ExpectedCredentialID: slot.CredentialID,
		Credential: grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal}, Action: "provider.grant.import",
	}, access.Fail(409, string(adapter)+"_import_stale", "The credential slot changed during import. Reload before retrying."))
}
