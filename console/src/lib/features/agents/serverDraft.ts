import { validated, type Validation } from '#lib/editing/validation.js';
import type { components } from '#lib/management/schema.js';
import type { MCPServer, MCPServerUpdate } from './api';
import {
  serverPricingForm,
  validateServerPricing,
  type ServerPricingField,
  type ServerPricingForm
} from './form';

type CreateRequest = components['schemas']['CreateMCPServerRequest'];

export type ServerDraft = {
  name: string;
  project: string;
  transport: CreateRequest['transport'];
  endpoint: string;
  enabled: boolean;
  credential: string;
  removeCredential: boolean;
  executable: string;
  digest: string;
  oauth: boolean;
  clientID: string;
  audience: string;
  scopes: string;
  authorizationEndpoint: string;
  tokenEndpoint: string;
  deviceEndpoint: string;
  entraIssuer: string;
  entraSecret: string;
  delegation: boolean;
  delegationIssuer: string;
  delegationAudience: string;
  privateKey: string;
  pricing: ServerPricingForm;
};

export type ServerField =
  | Exclude<
      keyof ServerDraft,
      'enabled' | 'removeCredential' | 'oauth' | 'delegation' | 'pricing'
    >
  | ServerPricingField;

export const serverFields: Readonly<Record<string, ServerField>> = {
  name: 'name',
  project_id: 'project',
  transport: 'transport',
  stdio: 'executable',
  endpoint: 'endpoint',
  credential: 'credential',
  'oauth.entra_obo.issuer': 'entraIssuer',
  entra_client_secret: 'entraSecret',
  delegation_private_key: 'privateKey',
  unit_price: 'unitPrice',
  calls_per_minute: 'callsPerMinute',
  max_concurrency: 'maxConcurrency',
  per_key_calls_per_minute: 'perKeyCallsPerMinute',
  per_key_max_concurrency: 'perKeyMaxConcurrency'
};

export const clearedSecrets: Pick<
  ServerDraft,
  'credential' | 'entraSecret' | 'privateKey'
> = { credential: '', entraSecret: '', privateKey: '' };

export function serverDraft(server: MCPServer | null): ServerDraft {
  const oauth = server?.oauth;
  return {
    name: server?.name ?? '',
    project: server?.project_id ?? '',
    transport: server?.transport ?? 'streamable_http',
    endpoint: server?.endpoint ?? '',
    enabled: server?.enabled ?? true,
    removeCredential: false,
    executable: server?.stdio?.executable ?? '',
    digest: server?.stdio?.digest ?? '',
    oauth: Boolean(oauth),
    clientID: oauth?.client_id ?? '',
    audience: oauth?.audience ?? '',
    scopes: oauth?.scopes.join(' ') ?? '',
    authorizationEndpoint: oauth?.authorization_endpoint ?? '',
    tokenEndpoint: oauth?.token_endpoint ?? '',
    deviceEndpoint: oauth?.device_authorization_endpoint ?? '',
    entraIssuer: oauth?.entra_obo?.issuer ?? '',
    delegation: Boolean(server?.delegation),
    delegationIssuer: server?.delegation?.issuer ?? '',
    delegationAudience: server?.delegation?.audience ?? '',
    ...clearedSecrets,
    pricing: server
      ? serverPricingForm(server)
      : {
          unitPrice: '',
          toolPrices: {},
          callsPerMinute: '60',
          maxConcurrency: '8',
          perKeyCallsPerMinute: '60',
          perKeyMaxConcurrency: '8'
        }
  };
}

function blankError(value: string, message: string) {
  return value.trim() ? undefined : message;
}

function urlError(value: string) {
  return URL.canParse(value) ? undefined : 'Enter a full URL.';
}

function optionalUrlError(value: string) {
  return value ? urlError(value) : undefined;
}

function executableErrors({ executable, digest }: ServerDraft) {
  return {
    executable: blankError(executable, 'Enter the approved executable.'),
    digest: /^[a-f0-9]{64}$/.test(digest)
      ? undefined
      : 'Enter 64 lowercase hexadecimal characters.'
  };
}

function oauthErrors(draft: ServerDraft) {
  if (!draft.oauth) return {};
  return {
    clientID: blankError(draft.clientID, 'Enter the client ID.'),
    audience: blankError(draft.audience, 'Enter the audience.'),
    scopes: blankError(draft.scopes, 'Enter at least one scope.'),
    authorizationEndpoint: optionalUrlError(draft.authorizationEndpoint),
    tokenEndpoint: urlError(draft.tokenEndpoint),
    deviceEndpoint: optionalUrlError(draft.deviceEndpoint)
  };
}

function delegationErrors(draft: ServerDraft) {
  if (!draft.delegation) return {};
  return {
    delegationIssuer: urlError(draft.delegationIssuer),
    delegationAudience: blankError(
      draft.delegationAudience,
      'Enter the audience.'
    )
  };
}

function connectionErrors(draft: ServerDraft, creating: boolean) {
  if (draft.transport === 'stdio')
    return creating ? executableErrors(draft) : {};
  return {
    endpoint: urlError(draft.endpoint),
    ...oauthErrors(draft),
    ...delegationErrors(draft)
  };
}

function oauthInput(draft: ServerDraft) {
  return {
    client_id: draft.clientID,
    audience: draft.audience,
    scopes: draft.scopes.split(/\s+/).filter(Boolean),
    token_endpoint: draft.tokenEndpoint,
    authorization_endpoint: draft.authorizationEndpoint || undefined,
    device_authorization_endpoint: draft.deviceEndpoint || undefined,
    entra_obo: draft.entraIssuer ? { issuer: draft.entraIssuer } : undefined
  };
}

function connectionInput(draft: ServerDraft) {
  if (draft.transport === 'stdio') return { endpoint: undefined };
  const entra = draft.oauth && draft.entraIssuer;
  return {
    endpoint: draft.endpoint,
    credential: draft.removeCredential ? null : draft.credential || undefined,
    oauth: draft.oauth ? oauthInput(draft) : null,
    delegation: draft.delegation
      ? { issuer: draft.delegationIssuer, audience: draft.delegationAudience }
      : null,
    delegation_private_key: draft.delegation
      ? draft.privateKey || undefined
      : undefined,
    entra_client_secret: entra ? draft.entraSecret || undefined : undefined
  };
}

type ServerRequests = { create: CreateRequest; update: MCPServerUpdate };

export function validateServer(
  draft: ServerDraft,
  current: MCPServer | null
): Validation<ServerRequests, ServerField> {
  const prices = validateServerPricing(
    current?.catalog.tools ?? [],
    draft.pricing
  );
  const fieldErrors = {
    name: blankError(draft.name, 'Enter a name.'),
    project: current
      ? undefined
      : blankError(draft.project, 'Choose a project.'),
    ...connectionErrors(draft, !current)
  };
  if (!prices.ok)
    return {
      ok: false,
      fieldErrors: { ...fieldErrors, ...prices.fieldErrors }
    };
  return validated<ServerRequests, ServerField>(fieldErrors, () => {
    const update = {
      ...prices.value,
      name: draft.name,
      enabled: draft.enabled,
      ...connectionInput(draft)
    };
    const { executable, digest } = draft;
    return {
      update,
      create: {
        ...update,
        project_id: draft.project,
        transport: draft.transport,
        ...(draft.transport === 'stdio'
          ? { stdio: { executable, digest } }
          : {})
      }
    };
  });
}
