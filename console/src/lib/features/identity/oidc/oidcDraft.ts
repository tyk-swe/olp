import { captureResult } from '#lib/editing/result.js';
import { validated, type Validation } from '#lib/editing/validation.js';
import { errorMessage } from '#lib/management/http.js';
import type { OidcConfiguration, OidcConfigurationInput } from './api';
import { parseRoleMappings } from './mappings';

export type OidcDraft = {
  discoveryUrl: string;
  issuer: string;
  clientId: string;
  clientSecret: string;
  enabled: boolean;
  scopes: string;
  emailClaim: string;
  groupsClaim: string;
  defaultRole: string;
  emailMappings: string;
  groupMappings: string;
};
export type OidcField = Exclude<keyof OidcDraft, 'enabled'>;

export const oidcServerFields: Readonly<Record<string, OidcField>> = {
  discovery_url: 'discoveryUrl',
  issuer: 'issuer',
  client_id: 'clientId',
  client_secret: 'clientSecret',
  scopes: 'scopes',
  email_claim: 'emailClaim',
  groups_claim: 'groupsClaim',
  default_role: 'defaultRole',
  email_role_mappings: 'emailMappings',
  group_role_mappings: 'groupMappings'
};

function mappingLines(mappings: OidcConfiguration['email_role_mappings']) {
  return mappings
    .map((mapping) => `${mapping.claim_value}=${mapping.role}`)
    .join('\n');
}

export function oidcDraft(saved?: OidcConfiguration | null): OidcDraft {
  return {
    discoveryUrl: saved?.discovery_url ?? '',
    issuer: saved?.issuer ?? '',
    clientId: saved?.client_id ?? '',
    clientSecret: '',
    enabled: saved?.enabled ?? false,
    scopes: saved?.scopes.join(' ') ?? 'openid profile email',
    emailClaim: saved?.email_claim ?? 'email',
    groupsClaim: saved?.groups_claim ?? 'groups',
    defaultRole: saved ? (saved.default_role ?? '') : 'viewer',
    emailMappings: mappingLines(saved?.email_role_mappings ?? []),
    groupMappings: mappingLines(saved?.group_role_mappings ?? [])
  };
}

export function validateOidc(
  draft: OidcDraft
): Validation<OidcConfigurationInput, OidcField> {
  const issuer = draft.issuer.trim();
  const discoveryUrl = draft.discoveryUrl.trim();
  const clientId = draft.clientId.trim();
  const email = captureResult(() => parseRoleMappings(draft.emailMappings));
  const groups = captureResult(() => parseRoleMappings(draft.groupMappings));
  const fieldErrors = {
    issuer: URL.canParse(issuer) ? undefined : 'Enter the issuer URL.',
    discoveryUrl: URL.canParse(discoveryUrl)
      ? undefined
      : 'Enter the discovery URL.',
    clientId: clientId ? undefined : 'Enter the client ID.',
    emailMappings: email.ok ? undefined : errorMessage(email.error),
    groupMappings: groups.ok ? undefined : errorMessage(groups.error)
  };
  if (!email.ok || !groups.ok) return { ok: false, fieldErrors };
  return validated<OidcConfigurationInput, OidcField>(fieldErrors, () => ({
    discovery_url: discoveryUrl,
    issuer,
    client_id: clientId,
    client_secret: draft.clientSecret || null,
    enabled: draft.enabled,
    scopes: draft.scopes.split(/[ ,]+/).filter(Boolean),
    email_claim: draft.emailClaim.trim() || 'email',
    groups_claim: draft.groupsClaim.trim() || 'groups',
    default_role: draft.defaultRole || null,
    email_role_mappings: email.value,
    group_role_mappings: groups.value
  }));
}
