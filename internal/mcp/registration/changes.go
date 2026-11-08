package registration

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type registrationChange struct{ id, etag, action string }

func (s *Server) prepareWrite(r *http.Request, id string, retire bool) (Input, error) {
	if retire {
		return Input{}, nil
	}
	var decoded Input
	if err := access.DecodeUnique(r, &decoded, 64<<10); err != nil {
		return Input{}, err
	}
	merged := func() Input {
		if id != "" {
			if current, err := Load(r.Context(), s.access.Pool, id, false); err == nil {
				return mergeAbsentPricingAndLimits(decoded, current)
			}
		}
		return decoded
	}()
	return Prepare(merged, s.egress)
}

func (s *Server) certificationCredential(ctx context.Context, in Input, previous *Definition) ([]byte, error) {
	if len(in.Credential) == 0 && previous != nil && previous.CredentialID != nil {
		if in.Endpoint != previous.Endpoint {
			return nil, access.Invalid("credential", "Supply a replacement credential or null when changing the MCP endpoint.")
		}
		material, err := s.access.Keys.Read(ctx, s.access.Pool, s.access.Installation, *previous.CredentialID, secrets.MCPCredential)
		if err != nil {
			return nil, access.Fail(422, "mcp_credential_unavailable", "The registered credential is unavailable.")
		}
		return material, nil
	}
	material, err := secrets.ParseBearer(in.Credential)
	if err != nil {
		return nil, access.Invalid("credential", err.Error())
	}
	return material, nil
}

func (s *Server) certifyChange(ctx context.Context, in Input, previous *Definition, retire, limitsOnly bool) ([]byte, *upstream.Catalog, error) {
	if retire {
		return nil, nil, nil
	}
	if limitsOnly {
		return nil, previous.Catalog, nil
	}
	material, err := s.certificationCredential(ctx, in, previous)
	if err != nil {
		return nil, nil, err
	}
	catalog, err := upstream.CertifyOptions(ctx, s.egress, in.Endpoint, material, upstream.TransportOptions{Transport: definitionTransport(in, previous), Stdio: definitionStdio(in, previous), Executables: s.executables})
	if err != nil {
		clear(material)
		return nil, nil, access.Fail(422, "mcp_certification_failed", "The bounded MCP handshake or tool-schema certification failed.")
	}
	return material, catalog, nil
}

func (s *Server) loadForWrite(ctx context.Context, tx pgx.Tx, principal access.Principal, in Input, id string) (*Definition, error) {
	if id == "" {
		return nil, s.access.RequireProject(ctx, tx, principal, in.ProjectID)
	}
	current, err := Load(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if err := principal.Project(&current.ProjectID, access.Change); err != nil {
		return nil, err
	}
	return current, nil
}

func inputLimits(in Input) routing.ToolLimits {
	return routing.EffectiveToolLimits(
		routing.ToolLimitOr(in.CallsPerMinute, routing.DefaultToolCallsPerMinute), routing.ToolLimitOr(in.MaxConcurrency, routing.DefaultToolMaxConcurrency),
		routing.ToolLimitOr(in.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute), routing.ToolLimitOr(in.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency))
}

func (s *Server) saveCredential(ctx context.Context, tx pgx.Tx, in Input, current *Definition, material []byte) (*string, error) {
	if len(in.Credential) == 0 && current != nil {
		return current.CredentialID, nil
	}
	if len(material) == 0 {
		return nil, nil
	}
	id := access.NewID()
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.MCPCredential, material, nil); err != nil {
		return nil, err
	}
	return &id, nil
}

func definitionFromInput(in Input, current *Definition, catalog *upstream.Catalog, credentialID, signingID, entraID *string) Definition {
	project := func() string {
		if current != nil {
			return current.ProjectID
		}
		return *in.ProjectID
	}()
	limits := inputLimits(in)
	return Definition{
		ProjectID: project, Name: in.Name, Endpoint: in.Endpoint, Enabled: *in.Enabled,
		Transport: string(definitionTransport(in, current)), Stdio: definitionStdio(in, current), Catalog: catalog,
		CredentialID: credentialID, DelegationKeyID: signingID, EntraClientSecretID: entraID,
		Delegation: in.Delegation.Clone(), OAuth: in.OAuth.Clone(), UnitPrice: clonePrice(in.UnitPrice), ToolPrices: cloneToolPrices(in.ToolPrices),
		CallsPerMinute: limits.CallsPerMinute, MaxConcurrency: limits.MaxConcurrency, PerKeyCallsPerMinute: limits.PerKeyCallsPerMinute, PerKeyMaxConcurrency: limits.PerKeyMaxConcurrency,
	}
}

func (s *Server) saveChange(ctx context.Context, tx pgx.Tx, actor string, in Input, current *Definition, catalog *upstream.Catalog, material []byte, retire, limitsOnly bool) (registrationChange, error) {
	if limitsOnly {
		change := registrationChange{id: current.ID, etag: access.NewID(), action: "mcp_server.update"}
		return change, UpdateLimits(ctx, tx, change.id, inputLimits(in), change.etag)
	}
	if retire {
		change := registrationChange{id: current.ID, etag: access.NewID(), action: "mcp_server.retire"}
		_, err := tx.Exec(ctx, "UPDATE olp.mcp_servers SET enabled=false,retired_at=now(),credential_id=NULL,delegation_key_id=NULL,entra_client_secret_id=NULL,etag=$2 WHERE id=$1", change.id, change.etag)
		return change, err
	}
	signingID, err := s.saveDelegationKey(ctx, tx, in, current)
	if err != nil {
		return registrationChange{}, err
	}
	entraID, err := s.saveEntraClientSecret(ctx, tx, in, current)
	if err != nil {
		return registrationChange{}, err
	}
	credentialID, err := s.saveCredential(ctx, tx, in, current, material)
	if err != nil {
		return registrationChange{}, err
	}
	candidate := definitionFromInput(in, current, catalog, credentialID, signingID, entraID)
	saved, err := Save(ctx, tx, actor, current, candidate)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return registrationChange{}, access.Fail(409, "mcp_server_exists", "Choose an unused MCP name in this project.")
		}
		return registrationChange{}, err
	}
	action := func() string {
		if current == nil {
			return "mcp_server.create"
		}
		return "mcp_server.update"
	}()
	return registrationChange{id: saved.ID, etag: saved.ETag, action: action}, nil
}

func DeleteReplacedCredentials(ctx context.Context, tx pgx.Tx, in Input, current *Definition, retire bool) error {
	if current == nil {
		return nil
	}
	for _, credential := range []struct {
		id      *string
		replace bool
		purpose secrets.SealPurpose
	}{
		{current.CredentialID, retire || len(in.Credential) != 0, secrets.MCPCredential},
		{current.DelegationKeyID, retire || in.Delegation == nil || in.DelegationPrivateKey != nil, secrets.MCPDelegationSigning},
		{current.EntraClientSecretID, retire || in.OAuth == nil || in.OAuth.EntraOBO == nil || in.EntraClientSecret != nil, secrets.MCPEntraClient},
	} {
		if credential.id == nil || !credential.replace {
			continue
		}
		if _, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *credential.id, credential.purpose); err != nil {
			return err
		}
	}
	return nil
}

func changeReply(ctx context.Context, tx pgx.Tx, change registrationChange, retire, created bool) (access.Reply, error) {
	if retire {
		return access.Reply{Status: http.StatusNoContent}, nil
	}
	next, err := Load(ctx, tx, change.id, false)
	if err != nil {
		return access.Reply{}, err
	}
	detail := access.Detail(next, change.etag)
	if !created {
		return detail, nil
	}
	return access.Reply{Status: http.StatusCreated, Location: "/api/v1/mcp-servers/" + change.id, ETag: detail.ETag, Body: detail.Body}, nil
}
